# `traceroute` — measure one hop of the path (Rust)

Every router a packet crosses subtracts one from its *time to live* (TTL). The
router where it reaches zero drops the packet and sends back an ICMP "time exceeded"
message. So a packet with TTL 5 is answered by the 5th router, and the time until
the answer is the round-trip time **to that router**.

With option `hop` = N this prober measures the Nth router, like one line of `mtr`.
With `hop` = 0 it measures the destination itself. Put one target per hop and the
charts show where along the path latency or loss starts.

It needs **no root and no capabilities**.

## Use it

```sh
make                      # cargo build --release, copies ./probe  (needs Rust)
cd .. && ./install.sh traceroute
```

| Option | Meaning |
|---|---|
| Hop | `0` (default): the destination. `N`: the Nth router. |
| UDP port | destination port of the probes, default 33434. It is almost always closed, which is what makes the destination answer "port unreachable". |

IPv4 and Linux only. Many routers do not answer or rate-limit these ICMP messages;
a hop that shows "loss" while later hops do not is usually that, not real loss.

## How it works

The classic traceroute needs a raw socket (root) to see the ICMP reply. On Linux, a
normal UDP socket can ask the kernel to keep the ICMP errors that belong to it in a
side queue, the **error queue**. [`src/udp_trace.rs`](src/udp_trace.rs) does, per
probe:

1. open a fresh UDP socket (a new source port, so answers to earlier probes cannot
   be mistaken for this one);
2. set its TTL;
3. `setsockopt(IP_RECVERR)`: keep ICMP errors;
4. `connect` to the destination, so only errors about this flow arrive;
5. send a few bytes and note the time;
6. `poll` for an error until the timeout;
7. `recvmsg(MSG_ERRQUEUE)` and read the ICMP type, code and the sender's address.

`means_reached` turns that into a verdict. A "port unreachable" from the destination
is a healthy answer; from anyone else it is a middlebox speaking for the host. A
"time exceeded" is the answer to a hop measurement. Any other "unreachable" means a
router said the packet cannot get through: loss.

### Rust points

- **`unsafe` is confined to one file**, each block carries a `SAFETY` comment saying
  why it is sound, and the rest of the program (`main.rs`) is entirely safe code.
- **Parse, don't validate.** `parse` turns JSON into a `Request` struct once. After
  that no function has to wonder whether a field exists or is in range.
- **Errors are values.** `Result<_, String>` carries the message that becomes an
  error answer; nothing panics on bad input (`rejects_bad_input_with_a_message_not_a_panic`).
- **Three outcomes, not two.** `probe_once` returns `Ok(Some(rtt))` (reached),
  `Ok(None)` (lost) or `Err(_)` (the system itself failed, so we cannot measure).
  The difference between the last two is the difference between loss and an error.

## Test it

```sh
make test       # cargo test --locked
```

The tests send to `127.0.0.1`: the kernel answers a closed UDP port with "port
unreachable", exactly as a real destination does. `a_bound_port_is_not_unreachable`
shows the opposite: a listening port gives no answer and counts as lost.

## Try changing it

- Print the whole path: loop `hop` from 1 and stop at the destination. Where would
  you send it, given a prober answers with only sent, received and the times?
- Add IPv6. Which socket options and ICMP types change?
- Use `SO_TIMESTAMP` for the receive time instead of `Instant::now()`. What error
  does that remove?
