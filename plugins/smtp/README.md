# `smtp` — time a mail server (Ruby)

Connects to an SMTP server, talks just far enough to reach the stage you ask for,
says `QUIT`, and reports how long that took. It never logs in and sends no mail, so
there is no password to keep. Standard library only; the code is in [`probe`](probe).

## Use it

```sh
cd .. && ./install.sh smtp
```

| Option | Meaning |
|---|---|
| Security | `none` (port 25), `starttls` (port 587), `tls` (port 465, TLS from the first byte) |
| Measure until | `banner`: the server's `220` greeting. `ehlo` (default): also a complete answer to `EHLO`, including TLS when it is used. |
| EHLO name | what we call ourselves; letters, digits, dots and hyphens |

Certificates are always verified, including the host name. Use a host name, not an
IP address, as the target if you want TLS checked; an address cannot match a
certificate, and the probe is then lost rather than silently unchecked.

## How it works

SMTP is a conversation of text lines ending in CRLF. Each server reply starts with
a three-digit code. A reply can span lines: `250-SIZE ...` (dash: more to come) up
to `250 OK` (space: last line).

- `read_reply` reads one complete reply, and every limit in it is deliberate:
  `gets("\n", MAX_LINE)` stops after 1 KB, there are at most 64 lines, and the code
  must stay the same on every line. A server may be broken or hostile; a prober
  that reads without a bound can be made to hang or to eat memory.
- `HELO_NAME` is a whitelist for text that goes **straight onto the wire**. Without
  it, a name containing `\r\n` could inject SMTP commands. This is the same bug as
  shell injection, in another protocol: never put unchecked text into a stream that
  something else will interpret.
- `start_tls` wraps the socket with verification **on**. `post_connection_check`
  matches the certificate to the host name.
- `one_probe` raises nothing for a bad server: `BadReply` and TLS errors become a
  lost probe. Timeouts, refused connections and unresolvable names are rescued in
  `measure`, and are also loss. Only a bad request is an error answer.
- The politeness of sending `QUIT` is **not** timed, and its failure is ignored.

## Test it

```sh
ruby test_probe.rb
```

`FakeSMTP` plays a server from a script. Read `test_unhealthy_replies_are_loss`
(a refusal, a non-SMTP greeting, a failed EHLO, a 5 KB line) and `TLSTest`, which
proves a self-signed certificate is **rejected**: the property most worth testing,
because a prober that quietly accepts any certificate reports healthy servers that
real clients would refuse.

## Try changing it

- Add a `check_starttls_offered` option: pass only if `STARTTLS` appears in the
  `EHLO` reply. You have to read the lines of a multi-line reply, not just the code.
- Measure `MAIL FROM:<>` and `RSET` as a deeper health check.
- Why is `Timeout.timeout` acceptable here, and what would you use instead for
  non-blocking code?
