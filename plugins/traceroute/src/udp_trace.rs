//! The one module that talks to the operating system. Linux only.
//!
//! THE PROBLEM. To see the ICMP message a router sends back ("time exceeded"), the
//! classic traceroute opens a RAW socket, which needs root. Linux offers a way
//! around that: a normal UDP socket can ask the kernel to keep the ICMP errors
//! that belong to it in a side queue, the "error queue", and we read them from
//! there. No special permission is needed.
//!
//! THE RECIPE, per probe:
//!   1. open a UDP socket (a new one each time: a fresh source port means answers
//!      to earlier probes can never be mistaken for this one)
//!   2. set its TTL                       -> decides which router answers
//!   3. ask for the error queue           -> setsockopt(IP_RECVERR)
//!   4. connect() it to the destination   -> only errors about THIS flow arrive
//!   5. send a few bytes, remember the time
//!   6. poll() until the socket reports an error, or the timeout passes
//!   7. read the error queue with recvmsg(MSG_ERRQUEUE) and look at the ICMP type
//!
//! The standard library covers steps 1, 2, 4 and 5. Steps 3, 6 and 7 are system
//! calls it does not wrap, so we use the `libc` crate. Those calls are `unsafe`
//! in Rust: the compiler cannot check what the kernel does with the memory we
//! hand it. Every `unsafe` block below says why it is sound.

use std::io;
use std::mem;
use std::net::{Ipv4Addr, SocketAddrV4, UdpSocket};
use std::os::fd::AsRawFd;
use std::time::{Duration, Instant};

/// TTL used to measure the destination itself: far more hops than any real path.
const DESTINATION_TTL: u32 = 64;

// ICMP message types and codes (RFC 792) that matter here.
const ICMP_DEST_UNREACH: u8 = 3;
const ICMP_PORT_UNREACH: u8 = 3; // code of ICMP_DEST_UNREACH
const ICMP_TIME_EXCEEDED: u8 = 11;
/// `ee_origin` value meaning "this error came from an ICMP message".
const SO_EE_ORIGIN_ICMP: u8 = 2;

/// What the error queue told us about one probe.
struct IcmpReply {
    icmp_type: u8,
    icmp_code: u8,
    from: Ipv4Addr, // the router (or host) that sent the ICMP message
}

/// Send one probe.
///
/// * `Ok(Some(rtt))` the probe was answered in a way that means "reached what we
///   aimed at": the round-trip time
/// * `Ok(None)`      no answer in time, or an answer that means "not reached":
///   packet loss as far as fogping is concerned
/// * `Err(_)`        the system call itself failed: we cannot measure
pub fn probe_once(dest: Ipv4Addr, port: u16, hop: u8, timeout: Duration) -> io::Result<Option<Duration>> {
    let socket = UdpSocket::bind("0.0.0.0:0")?;
    let ttl = if hop == 0 { DESTINATION_TTL } else { u32::from(hop) };
    socket.set_ttl(ttl)?;
    enable_error_queue(&socket)?;
    socket.connect(SocketAddrV4::new(dest, port))?;

    let started = Instant::now();
    socket.send(b"fogping-traceroute")?;

    let remaining = match timeout.checked_sub(started.elapsed()) {
        Some(r) => r,
        None => return Ok(None),
    };
    if !wait_for_error(&socket, remaining)? {
        return Ok(None); // nothing came back in time
    }
    let rtt = started.elapsed(); // stop the clock as soon as the answer is there

    let Some(reply) = read_error_queue(&socket)? else {
        return Ok(None);
    };
    Ok(means_reached(&reply, dest, hop).then_some(rtt))
}

/// Decide whether this ICMP message is the answer we were hoping for.
fn means_reached(reply: &IcmpReply, dest: Ipv4Addr, hop: u8) -> bool {
    match (reply.icmp_type, reply.icmp_code) {
        // "Port unreachable" from the destination itself: the packet got all the
        // way there and the host said nobody listens. That is a healthy answer.
        // From anyone else it is a middlebox speaking for the host: not reached.
        (ICMP_DEST_UNREACH, ICMP_PORT_UNREACH) => (hop == 0 && reply.from == dest) || hop != 0,
        // "TTL exceeded": a router on the path dropped the packet. Exactly what
        // we want when measuring a hop; for the destination it means the path is
        // longer than 64 hops, which is not an answer from the destination.
        (ICMP_TIME_EXCEEDED, _) => hop != 0,
        // Any other "unreachable" (no route, administratively prohibited, ...): a
        // router answered, but to say the packet cannot get through. Not reached.
        _ => false,
    }
}

/// setsockopt(IPPROTO_IP, IP_RECVERR, 1): keep ICMP errors for us to read.
fn enable_error_queue(socket: &UdpSocket) -> io::Result<()> {
    let on: libc::c_int = 1;
    // SAFETY: the fd is a valid open socket owned by `socket` for the whole call;
    // `on` lives on the stack until the call returns and the length passed is
    // exactly its size.
    let rc = unsafe {
        libc::setsockopt(
            socket.as_raw_fd(),
            libc::IPPROTO_IP,
            libc::IP_RECVERR,
            &on as *const libc::c_int as *const libc::c_void,
            mem::size_of::<libc::c_int>() as libc::socklen_t,
        )
    };
    if rc == 0 { Ok(()) } else { Err(io::Error::last_os_error()) }
}

/// Wait until the socket has an error to report. poll() always reports POLLERR,
/// whether or not it was asked for. Returns false on timeout.
fn wait_for_error(socket: &UdpSocket, timeout: Duration) -> io::Result<bool> {
    let mut fds = libc::pollfd { fd: socket.as_raw_fd(), events: 0, revents: 0 };
    // Round up so a 0.4 ms timeout does not become 0 and return at once.
    let millis = timeout.as_millis().saturating_add(1).min(i32::MAX as u128) as libc::c_int;
    loop {
        // SAFETY: `fds` is one valid pollfd and we pass a count of 1.
        let rc = unsafe { libc::poll(&mut fds, 1, millis) };
        if rc > 0 {
            return Ok(fds.revents & libc::POLLERR != 0);
        }
        if rc == 0 {
            return Ok(false);
        }
        let err = io::Error::last_os_error();
        if err.kind() != io::ErrorKind::Interrupted {
            return Err(err);
        }
        // Interrupted by a signal: poll again.
    }
}

/// Take one message from the error queue and pick the ICMP details out of it.
fn read_error_queue(socket: &UdpSocket) -> io::Result<Option<IcmpReply>> {
    // The kernel copies the original packet's payload into `payload`, and the
    // error description into the "control message" buffer `control`. Both are
    // generously sized; a message that does not fit is truncated, never overrun.
    let mut payload = [0u8; 128];
    let mut control = [0u8; 512];
    let mut iov = libc::iovec { iov_base: payload.as_mut_ptr().cast(), iov_len: payload.len() };

    // SAFETY: a zeroed msghdr is a valid "empty" one; we then fill in only the
    // fields we use, each pointing at a buffer that outlives the recvmsg call.
    let mut msg: libc::msghdr = unsafe { mem::zeroed() };
    msg.msg_iov = &mut iov;
    msg.msg_iovlen = 1;
    msg.msg_control = control.as_mut_ptr().cast();
    msg.msg_controllen = control.len() as _;

    // MSG_ERRQUEUE: read from the error queue instead of the normal data queue.
    // MSG_DONTWAIT: never block here; poll() already told us there is something.
    // SAFETY: `msg` points at buffers that are alive and sized as declared.
    let rc = unsafe { libc::recvmsg(socket.as_raw_fd(), &mut msg, libc::MSG_ERRQUEUE | libc::MSG_DONTWAIT) };
    if rc < 0 {
        let err = io::Error::last_os_error();
        // EAGAIN: the error was already consumed; treat as "no answer".
        return if err.kind() == io::ErrorKind::WouldBlock { Ok(None) } else { Err(err) };
    }

    // Walk the control messages looking for the IP_RECVERR one.
    // SAFETY: CMSG_* only read within msg_control/msg_controllen, which the kernel
    // has just filled in and which we sized above.
    unsafe {
        let mut cmsg = libc::CMSG_FIRSTHDR(&msg);
        while !cmsg.is_null() {
            if (*cmsg).cmsg_level == libc::IPPROTO_IP && (*cmsg).cmsg_type == libc::IP_RECVERR {
                // The data is a `sock_extended_err`, immediately followed by the
                // address of whoever sent the ICMP message (a sockaddr_in).
                let data = libc::CMSG_DATA(cmsg);
                let err: libc::sock_extended_err = std::ptr::read_unaligned(data.cast());
                if err.ee_origin != SO_EE_ORIGIN_ICMP {
                    return Ok(None); // a local error, not an ICMP message from the network
                }
                let offender: libc::sockaddr_in =
                    std::ptr::read_unaligned(data.add(mem::size_of::<libc::sock_extended_err>()).cast());
                return Ok(Some(IcmpReply {
                    icmp_type: err.ee_type,
                    icmp_code: err.ee_code,
                    // s_addr is in network byte order; from_be turns it into the
                    // number Ipv4Addr::from expects.
                    from: Ipv4Addr::from(u32::from_be(offender.sin_addr.s_addr)),
                }));
            }
            cmsg = libc::CMSG_NXTHDR(&msg, cmsg);
        }
    }
    Ok(None)
}
