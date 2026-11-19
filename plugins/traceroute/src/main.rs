//! A fogping prober that measures one hop of the network path. Written in Rust.
//!
//! This is a UDP traceroute. It sends small UDP packets with a limited "time to
//! live" (TTL). Every router on the way subtracts one from the TTL, and the router
//! where it reaches zero throws the packet away and sends back an ICMP "time
//! exceeded" message. So a packet with TTL 5 is answered by the 5th router, and
//! the time until that answer arrives is the round-trip time to that router.
//!
//! * option `hop` = 0: measure the destination itself (a packet with a big TTL,
//!   answered by an ICMP "port unreachable" from the host)
//! * option `hop` = N: measure the Nth router, like one line of `mtr`
//!
//! Why this makes a good fogping target: put one target per hop and the charts
//! show where along the path latency or loss starts.
//!
//! It needs NO root and no capabilities: see udp_trace.rs for how.
//!
//! HOW A PROBER TALKS TO FOGPING (the same in every language)
//!
//! 1. read ONE JSON object from standard input (the request)
//! 2. measure
//! 3. write ONE JSON object to standard output (the answer)
//!
//! Nothing arrives on the command line and the environment is nearly empty. The
//! complete contract is docs/PROBE-PROTOCOL.md.
//!
//! Try it by hand:
//!   echo '{"protocol":"fogping-probe/1","type":"traceroute","host":"127.0.0.1",
//!          "packets":3,"gap_ms":100,"timeout_ms":1000,"options":{"hop":0}}' | ./probe

mod udp_trace;

use serde_json::{json, Value};
use std::io::Read;
use std::net::{Ipv4Addr, ToSocketAddrs};
use std::time::Duration;

const PROTOCOL: &str = "fogping-probe/1";

/// A validated request. Parsing turns loosely typed JSON into this once, so the
/// rest of the program never has to wonder whether a field exists or has the
/// right type. That is the Rust way: make invalid states unrepresentable.
struct Request {
    host: String,
    packets: u32,
    gap: Duration,
    timeout: Duration,
    hop: u8,
    udp_port: u16,
}

/// Read an integer field and check its range. Returns a message for the user
/// (which becomes an `error` answer) rather than panicking.
fn int_in(v: &Value, key: &str, lo: i64, hi: i64) -> Result<i64, String> {
    let n = v
        .get(key)
        .and_then(Value::as_i64)
        .ok_or_else(|| format!("{key} must be a whole number"))?;
    if n < lo || n > hi {
        return Err(format!("{key} must be {lo}-{hi}"));
    }
    Ok(n)
}

fn parse(raw: &str) -> Result<Request, String> {
    let v: Value = serde_json::from_str(raw).map_err(|e| format!("bad request: {e}"))?;
    if v.get("protocol").and_then(Value::as_str) != Some(PROTOCOL) {
        return Err("unsupported protocol".into());
    }
    let host = v
        .get("host")
        .and_then(Value::as_str)
        .filter(|h| valid_host(h))
        .ok_or("host must be a hostname or an IP address")?
        .to_string();

    // fogping has already checked the options against manifest.json and filled in
    // the defaults, but a prober never relies on that for its own safety: `options`
    // is an object, and these two keys might in principle be absent.
    let empty = json!({});
    let opts = v.get("options").unwrap_or(&empty);
    let hop = opts.get("hop").map_or(Ok(0), |_| int_in(opts, "hop", 0, 64))?;
    let udp_port = opts
        .get("udp_port")
        .map_or(Ok(33434), |_| int_in(opts, "udp_port", 1, 65535))?;

    Ok(Request {
        host,
        packets: int_in(&v, "packets", 1, 1000)? as u32,
        gap: Duration::from_millis(int_in(&v, "gap_ms", 0, 600_000)? as u64),
        timeout: Duration::from_millis(int_in(&v, "timeout_ms", 1, 600_000)? as u64),
        hop: hop as u8,
        udp_port: udp_port as u16,
    })
}

/// Hostnames and IP addresses only: letters, digits, `.`, `-`, `_`, `:`. A
/// whitelist, so nothing unexpected can slip through.
fn valid_host(h: &str) -> bool {
    !h.is_empty()
        && h.len() <= 253
        && h.chars().all(|c| c.is_ascii_alphanumeric() || ".-_:".contains(c))
}

/// This prober speaks IPv4 only (the ICMP messages it relies on differ in IPv6).
fn resolve_v4(host: &str, port: u16) -> Result<Ipv4Addr, String> {
    let mut addrs = (host, port)
        .to_socket_addrs()
        .map_err(|e| format!("cannot resolve the host: {e}"))?;
    addrs
        .find_map(|a| match a.ip() {
            std::net::IpAddr::V4(v4) => Some(v4),
            std::net::IpAddr::V6(_) => None,
        })
        .ok_or_else(|| "the host has no IPv4 address (this prober is IPv4 only)".into())
}

/// Run the round. `Ok` is a measurement; `Err` is "could not measure at all".
fn measure(req: &Request) -> Result<Value, String> {
    let dest = resolve_v4(&req.host, req.udp_port)?;

    let mut rtts: Vec<f64> = Vec::new();
    for i in 0..req.packets {
        if i > 0 {
            std::thread::sleep(req.gap);
        }
        match udp_trace::probe_once(dest, req.udp_port, req.hop, req.timeout) {
            Ok(Some(rtt)) => rtts.push((rtt.as_secs_f64() * 1_000_000.0).round() / 1000.0),
            // No answer in time, or an answer that does not mean "reached": loss.
            Ok(None) => {}
            // The system itself refused (no permission, no route): we cannot
            // measure, which is different from the network dropping packets.
            Err(e) => return Err(format!("cannot probe: {e}")),
        }
    }
    // Always sent, received and ms together. Lost probes are not in the list.
    Ok(json!({
        "protocol": PROTOCOL,
        "sent": req.packets,
        "received": rtts.len(),
        "ms": rtts,
    }))
}

fn main() {
    // Read at most 64 KB of the request.
    let mut raw = String::new();
    let answer = match std::io::stdin().take(64 * 1024).read_to_string(&mut raw) {
        Err(e) => failure(&format!("cannot read the request: {e}")),
        Ok(_) => match parse(&raw).and_then(|req| measure(&req)) {
            Ok(measurement) => measurement,
            Err(message) => failure(&message),
        },
    };
    println!("{answer}");
}

/// We could not measure at all. fogping records this as a failed round, not as
/// packet loss.
fn failure(message: &str) -> Value {
    json!({ "protocol": PROTOCOL, "error": message })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(extra: &str) -> String {
        format!(
            r#"{{"protocol":"fogping-probe/1","type":"traceroute","host":"127.0.0.1",
               "packets":2,"gap_ms":1,"timeout_ms":500,"options":{extra}}}"#
        )
    }

    #[test]
    fn parses_a_good_request_and_fills_defaults() {
        let r = parse(&request("{}")).unwrap();
        assert_eq!((r.hop, r.udp_port, r.packets), (0, 33434, 2));
    }

    #[test]
    fn rejects_bad_input_with_a_message_not_a_panic() {
        for bad in [
            request(r#"{"hop":65}"#),
            request(r#"{"hop":"3"}"#),
            request(r#"{"udp_port":0}"#),
            r#"{"protocol":"fogping-probe/9"}"#.to_string(),
            "not json".to_string(),
            request("{}").replace("127.0.0.1", "a b"),
            request("{}").replace("127.0.0.1", "a;rm"),
        ] {
            assert!(parse(&bad).is_err(), "accepted: {bad}");
        }
    }

    #[test]
    fn destination_mode_reaches_loopback() {
        // Nothing listens on this UDP port, so the kernel answers "port
        // unreachable": exactly what a real destination does to a traceroute.
        let a = measure(&parse(&request(r#"{"udp_port":33434}"#)).unwrap()).unwrap();
        assert_eq!(a["sent"], 2);
        assert_eq!(a["received"], 2, "{a}");
        assert_eq!(a["ms"].as_array().unwrap().len(), 2);
    }

    #[test]
    fn a_bound_port_is_not_unreachable() {
        // A UDP socket that is listening does not generate "port unreachable",
        // so the probe gets no answer and counts as lost.
        let s = std::net::UdpSocket::bind("127.0.0.1:0").unwrap();
        let port = s.local_addr().unwrap().port();
        let a = measure(&parse(&request(&format!(r#"{{"udp_port":{port}}}"#))).unwrap()).unwrap();
        assert_eq!(a["received"], 0, "{a}");
        assert_eq!(a["ms"], json!([]));
    }
}
