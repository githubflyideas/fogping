# `dns` — time a DNS server (Python)

Asks a DNS server for a name over UDP and times the reply. The target's **host is
the DNS server** (a resolver like `1.1.1.1`, or your authoritative server); the
option `query` is the name asked. No library: the DNS message is built and read by
hand in [`probe`](probe), which is the point of this lesson.

## Use it

```sh
cd .. && ./install.sh dns
```

Target type DNS, host `1.1.1.1`, port `53`, option Name to look up `example.com`.

| Option | Meaning |
|---|---|
| Name to look up | required |
| Record type | `A` (default), `AAAA`, `MX`, `NS`, `TXT`, `CNAME`, `SOA` |
| Require an answer | on (default): an empty answer (NODATA) counts as lost |

## How it works

A DNS message is a 12-byte header and a question.

```
header   ID | flags | questions | answers | authority | additional    six 16-bit numbers
question name: 3 www 7 example 3 com 0   |  type  |  class
```

- `encode_name` writes a name as length-prefixed labels ending in a zero.
- `build_query` packs the header with `struct.pack("!6H", ...)`. The `!` means
  network byte order. The random ID is how a reply is matched to its question.
- `check_reply` is the security-relevant part. Anything on the network can send us
  a UDP packet, so a reply counts only if it is long enough, carries **our ID**, has
  the response bit set, and has response code 0.
  - A packet that is *not an answer to us* raises `NotOurs`: it is ignored and we
    keep waiting. Treating a stray packet as a failed probe would let anyone who
    can send us UDP turn a healthy server into "lost".
  - A real answer with an error code is a lost probe: the server is not serving.
- `one_probe` uses **one deadline** for the whole probe and recomputes the time
  left on every receive. With a plain `settimeout`, a stream of junk packets could
  keep it waiting for ever, because each receive restarts the timer.
- The server's own address is resolved once, outside the timed section: we time
  the DNS server, not the lookup of the DNS server.

## Test it

```sh
python3 -m unittest test_probe.py
```

The tests run a fake DNS server on localhost that can answer well, answer with no
data, answer with an error, or answer with the wrong ID. Read `FakeServer`: writing
a fake peer is the quickest way to learn a protocol, and the best way to test a
prober.

## Try changing it

- Add `use_tcp`. DNS over TCP prefixes each message with a 2-byte length. What
  must you bound before you trust that length?
- Add `expect_ip`: count the probe only if the answer contains that address. You
  will need to parse compressed names; read RFC 1035 section 4.1.4.
- Send an EDNS0 option so large answers fit. What changes in `MAX_REPLY`?
