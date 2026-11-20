package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// These tests show how to test a prober that speaks a binary protocol: write a
// tiny fake server that speaks just enough of it, then feed run() a request and
// read the answer. Nothing leaves the machine.

// ---- a fake LDAP server ----

// An LDAP message is SEQUENCE { messageID INTEGER, protocolOp [APPLICATION n] ... }.
// The server only has to answer three operations: bind (0), search (3), unbind (2).

type fake struct {
	bindResult int64 // LDAP result code for a bind: 0 success, 49 invalid credentials
	rootDSE    bool  // whether a search returns the root DSE entry
	silent     bool  // accept connections and never answer
	listener   net.Listener
}

func response(id int64, op ber.Tag, resultCode int64) *ber.Packet {
	msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "message")
	msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "messageID"))
	body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, op, nil, "response")
	body.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, resultCode, "resultCode"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnostic"))
	msg.AppendChild(body)
	return msg
}

func entry(id int64) *ber.Packet {
	msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "message")
	msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "messageID"))
	body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 4, nil, "searchEntry")
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "objectName"))
	body.AppendChild(ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes"))
	msg.AppendChild(body)
	return msg
}

func (f *fake) serve(c net.Conn) {
	defer c.Close()
	if f.silent {
		// Read and discard everything the client sends, and never answer. (Returning
		// after the first read would close the connection, and the client would see
		// a closed connection instead of silence.)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, c)
		return
	}
	for {
		p, err := ber.ReadPacket(c)
		if err != nil {
			return
		}
		id, _ := p.Children[0].Value.(int64)
		switch p.Children[1].Tag {
		case 0: // bind request
			c.Write(response(id, 1, f.bindResult).Bytes())
		case 3: // search request
			if f.rootDSE {
				c.Write(entry(id).Bytes())
			}
			c.Write(response(id, 5, 0).Bytes())
		case 2: // unbind: hang up
			return
		}
	}
}

func start(t *testing.T, f *fake, wrap func(net.Listener) net.Listener) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		ln = wrap(ln)
	}
	f.listener = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// ---- helpers ----

// result is a loose view of an answer, so a test can tell "field absent" (nil)
// from "field is zero".
type result struct {
	Sent     *int            `json:"sent"`
	Received *int            `json:"received"`
	MS       json.RawMessage `json:"ms"`
	Error    string          `json:"error"`
}

func runReq(t *testing.T, host string, port int, opts string) result {
	t.Helper()
	body := `{"protocol":"fogping-probe/1","type":"ldap","host":"` + host + `","port":` + strconv.Itoa(port) +
		`,"packets":2,"gap_ms":1,"timeout_ms":600,"options":` + opts + `}`
	var out bytes.Buffer
	run(strings.NewReader(body), &out)
	var r result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, out.String())
	}
	return r
}

// ---- tests ----

func TestStages(t *testing.T) {
	port := start(t, &fake{rootDSE: true}, nil)
	for _, stage := range []string{"connect", "bind", "rootdse"} {
		r := runReq(t, "127.0.0.1", port, `{"stage":"`+stage+`"}`)
		if r.Error != "" || *r.Sent != 2 || *r.Received != 2 {
			t.Errorf("%s: %+v", stage, r)
		}
	}
}

func TestUnhealthyServersAreLoss(t *testing.T) {
	// Each of these is a server that is up but not serving properly: the probe is
	// LOST, and the answer is still a measurement with ms=[] (never an error).
	for name, f := range map[string]*fake{
		"bind refused":      {bindResult: 49, rootDSE: true},
		"no root DSE":       {rootDSE: false},
		"accepts, no reply": {silent: true},
	} {
		port := start(t, f, nil)
		t0 := time.Now()
		r := runReq(t, "127.0.0.1", port, `{"stage":"rootdse"}`)
		if r.Error != "" || *r.Received != 0 || string(r.MS) != "[]" {
			t.Errorf("%s: %+v ms=%s", name, r, r.MS)
		}
		if name == "accepts, no reply" {
			// 2 probes x 600 ms: the wait must have happened (it really was silence)
			// and it must have been bounded (the deadline held).
			if d := time.Since(t0); d < 1100*time.Millisecond || d > 3*time.Second {
				t.Errorf("%s: took %v, want about 1.2s", name, d)
			}
		}
	}
	r := runReq(t, "127.0.0.1", 1, `{}`) // nothing listening
	if r.Error != "" || *r.Received != 0 {
		t.Errorf("refused connection: %+v", r)
	}
}

// A prober that quietly accepted any certificate would report a directory healthy
// that real clients refuse. The most important TLS test is that a bad one fails.
func TestUntrustedCertificateIsLoss(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	port := start(t, &fake{rootDSE: true}, func(l net.Listener) net.Listener {
		return tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})
	})
	r := runReq(t, "127.0.0.1", port, `{"security":"ldaps","stage":"connect"}`)
	if r.Error != "" || *r.Received != 0 {
		t.Fatalf("a self-signed certificate was accepted: %+v", r)
	}
}

func TestOwnFailuresAreErrorsNotLoss(t *testing.T) {
	for name, body := range map[string]string{
		"hostile host":   `{"protocol":"fogping-probe/1","host":"a/b","port":389,"packets":1,"gap_ms":1,"timeout_ms":100,"options":{}}`,
		"bad port":       `{"protocol":"fogping-probe/1","host":"h","port":0,"packets":1,"gap_ms":1,"timeout_ms":100,"options":{}}`,
		"bad stage":      `{"protocol":"fogping-probe/1","host":"h","port":389,"packets":1,"gap_ms":1,"timeout_ms":100,"options":{"stage":"x"}}`,
		"bad security":   `{"protocol":"fogping-probe/1","host":"h","port":389,"packets":1,"gap_ms":1,"timeout_ms":100,"options":{"security":"x"}}`,
		"wrong protocol": `{"protocol":"fogping-probe/9"}`,
		"garbage":        `not json`,
	} {
		var out bytes.Buffer
		run(strings.NewReader(body), &out)
		var r result
		json.Unmarshal(out.Bytes(), &r)
		// An error answer has an error and NO measurement fields at all.
		if r.Error == "" || r.Sent != nil || r.Received != nil || r.MS != nil {
			t.Errorf("%s: want an error-only answer, got %s", name, out.String())
		}
	}
}
