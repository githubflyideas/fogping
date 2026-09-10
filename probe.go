package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// Round 是 fogping 的原子数据单元:一轮探测的全部原始样本。
// 存分布而不是均值 —— 这是整个项目的立项理由。
type Round struct {
	T  int64     `json:"t"`           // unix 秒
	S  int       `json:"s"`           // 发出
	R  int       `json:"r"`           // 收到
	MS []float64 `json:"ms"`          // RTT 样本(毫秒)
	B  bool      `json:"b,omitempty"` // flagged as a loss burst
	Z  float64   `json:"z,omitempty"` // robust z-score behind the flag (tooltip evidence)
}

// probeParams 解析目标的探测节奏。优先级:显式 interval_sec > pace 档位 > 全局默认。
// fast 档同时把每轮包数提到 30,分布更细 —— 快节奏链路值得更好的分辨率。
func probeParams(t TargetCfg, p ProbeCfg) (interval time.Duration, packets int) {
	iv, pk := p.IntervalSec, p.Packets
	switch t.Pace {
	case "fast":
		iv, pk = 15, 30
	case "slow":
		iv = 300
	}
	if t.IntervalSec > 0 {
		iv = t.IntervalSec
	}
	return time.Duration(iv) * time.Second, pk
}

func probeLoop(t TargetCfg, p ProbeCfg, store *Store, det *Detector, stop chan struct{}) {
	interval, packets := probeParams(t, p)
	gap := time.Duration(p.GapMs) * time.Millisecond
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var r Round
		var err error
		switch t.Type {
		case "icmp":
			r, err = icmpRound(t.Host, packets, gap, timeout)
		case "tcp":
			r, err = tcpRound(t.Host, t.Port, packets, gap, timeout)
		}
		if err != nil {
			log.Printf("[%s] probe error: %v", t.Name, err)
			r = Round{T: time.Now().Unix(), S: packets, R: 0} // 解析失败按全丢记录,不留时间空洞
		}
		r.B, r.Z = det.CheckBurst(t.Name, r) // verdict + z evidence, written with the round
		if err := store.Append(t.Name, r); err != nil {
			log.Printf("[%s] write error: %v", t.Name, err)
		}

		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

// ---- ICMP:纯 syscall,无第三方依赖 ----
// 优先 SOCK_DGRAM(无需 root,需 sysctl ping_group_range 允许),
// 失败回退 SOCK_RAW(需要 root 或 cap_net_raw)。

const icmpMagic = "fogpingv1!"

func icmpRound(host string, packets int, gap, timeout time.Duration) (Round, error) {
	r := Round{T: time.Now().Unix(), S: packets}
	ip, err := resolveIPv4(host)
	if err != nil {
		return r, err
	}

	fd, raw, err := icmpSocket()
	if err != nil {
		return r, fmt.Errorf("无法创建 ICMP socket(需要 ping_group_range 或 cap_net_raw): %w", err)
	}
	defer syscall.Close(fd)

	id := os.Getpid() & 0xffff
	var nonce [4]byte
	rand.Read(nonce[:])
	dst := &syscall.SockaddrInet4{Addr: ip}

	sendAt := make([]time.Time, packets)
	got := make([]bool, packets)

	for i := 0; i < packets; i++ {
		pkt := buildEcho(id, i, nonce)
		sendAt[i] = time.Now()
		if err := syscall.Sendto(fd, pkt, 0, dst); err != nil {
			continue // 单包发送失败不终止整轮
		}
		collect(fd, raw, id, nonce, sendAt, got, &r, time.Now().Add(gap))
	}
	// 尾窗:等最后一批在途包
	collect(fd, raw, id, nonce, sendAt, got, &r, time.Now().Add(timeout))
	return r, nil
}

func icmpSocket() (fd int, raw bool, err error) {
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err == nil {
		return fd, false, nil
	}
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	return fd, true, err
}

func buildEcho(id, seq int, nonce [4]byte) []byte {
	p := make([]byte, 8+len(icmpMagic)+4)
	p[0] = 8 // echo request
	binary.BigEndian.PutUint16(p[4:6], uint16(id))
	binary.BigEndian.PutUint16(p[6:8], uint16(seq))
	copy(p[8:], icmpMagic)
	copy(p[8+len(icmpMagic):], nonce[:])
	cs := icmpChecksum(p)
	binary.BigEndian.PutUint16(p[2:4], cs)
	return p
}

// collect 在 deadline 前持续收包,匹配到未回收的 seq 就记 RTT。
// 乱序、迟到的回包都能被后续窗口捞回,RTT 按各自 sendAt 计算,不失真。
func collect(fd int, raw bool, id int, nonce [4]byte, sendAt []time.Time, got []bool, r *Round, deadline time.Time) {
	buf := make([]byte, 1500)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return
		}
		wait := remain
		if wait > 50*time.Millisecond {
			wait = 50 * time.Millisecond // 50ms 一跳,保证按时退出
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		tv := syscall.NsecToTimeval(wait.Nanoseconds()) // 可移植:linux/darwin 的 Timeval 字段类型不同
		syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		now := time.Now()
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || err == syscall.EINTR {
				continue
			}
			return
		}
		pkt := buf[:n]
		if raw && n >= 20 { // raw socket 带 IP 头,剥掉
			ihl := int(pkt[0]&0x0f) * 4
			if n <= ihl {
				continue
			}
			pkt = pkt[ihl:n]
		}
		if len(pkt) < 8+len(icmpMagic)+4 || pkt[0] != 0 { // 只认 echo reply
			continue
		}
		// raw socket 会收到本机所有 ICMP 回包,靠 id+nonce 隔离并发目标
		if raw && int(binary.BigEndian.Uint16(pkt[4:6])) != id {
			continue
		}
		if string(pkt[8:8+len(icmpMagic)]) != icmpMagic ||
			string(pkt[8+len(icmpMagic):8+len(icmpMagic)+4]) != string(nonce[:]) {
			continue
		}
		seq := int(binary.BigEndian.Uint16(pkt[6:8]))
		if seq < 0 || seq >= len(sendAt) || got[seq] {
			continue
		}
		got[seq] = true
		r.R++
		r.MS = append(r.MS, float64(now.Sub(sendAt[seq]).Microseconds())/1000.0)
	}
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 > 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func resolveIPv4(host string) ([4]byte, error) {
	var out [4]byte
	ips, err := net.LookupIP(host)
	if err != nil {
		return out, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			copy(out[:], v4)
			return out, nil
		}
	}
	return out, fmt.Errorf("%s 没有 IPv4 地址(v1 仅支持 IPv4)", host)
}

// ---- TCP:connect 耗时即 RTT 近似 ----
// Resolve once per round and dial the IP: Go doesn't cache DNS, so dialing a
// hostname would add a lookup to every sample and measure the resolver, not the link.

func tcpRound(host string, port, packets int, gap, timeout time.Duration) (Round, error) {
	r := Round{T: time.Now().Unix(), S: packets}
	ips, err := net.LookupIP(host)
	if err != nil {
		return r, err
	}
	if len(ips) == 0 {
		return r, fmt.Errorf("%s: no address", host)
	}
	addr := net.JoinHostPort(ips[0].String(), strconv.Itoa(port))
	for i := 0; i < packets; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err == nil {
			r.R++
			r.MS = append(r.MS, float64(time.Since(t0).Microseconds())/1000.0)
			c.Close()
		}
		if i < packets-1 {
			time.Sleep(gap)
		}
	}
	return r, nil
}
