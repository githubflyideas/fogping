# `http` — time an HTTP(S) request (Go)

Measures how long a web server takes to start answering: DNS lookup, TCP connect,
TLS handshake and the server's own processing, up to the arrival of the response
**headers**. That is "time to first byte", the number users feel.

Read [`main.go`](main.go) top to bottom: it is about 300 lines and is the reference
for the structure every prober follows.

## Use it

```sh
make                                  # builds ./probe (standard library only)
cd .. && ./install.sh http
```

Then add a target of type HTTP, host `example.com`, port `443`.

| Option | Meaning |
|---|---|
| Scheme | `https` (default, certificate verified) or `http` |
| Method | `GET` (default) or `HEAD` |
| Path | starts with `/`; a query string is allowed |
| Host header | ask for another site on the same address |
| Expected status | `0` (default): any status below 400 is up. Otherwise exactly this status. |
| Follow redirects | off: a `301` is the answer. On: follow up to 5 and time the chain. |

There is deliberately no "ignore certificate errors" option. A monitor that says
"up" for a site your visitors' browsers refuse is lying.

## How it works

1. `run` reads the request, bounded to 64 KB, and checks the protocol version.
2. `parseOptions` turns the loosely typed request into a `config`. Rules only this
   prober knows (the path starts with `/`) live here, because fogping only checks
   what the manifest can express.
3. `validHost` is a **whitelist**. fogping accepts any host without spaces, so a
   host like `good.example/@evil.example` would, glued into a URL, make the probe
   talk to a different server. The first version of this prober had exactly that
   bug; the test `TestOwnFailuresAreErrorsNotLoss` now guards it.
4. `probe` runs `packets` requests. `DisableKeepAlives` makes each one open a new
   connection: with reuse, probes 2…N would skip DNS, TCP and TLS and measure
   something much smaller than probe 1.
5. A request that fails, times out or returns an unacceptable status has no entry
   in the result: that is loss. A `500` counts as loss because the service is not
   working, even though the network is fine.
6. Two answer types, `measurement` and `failure`, make the "both at once" mistake
   impossible to write. A first draft used one struct with `omitempty` and printed
   `{"sent":2,"received":0}`, with **no `ms`**, whenever everything was lost;
   fogping rejected it. `ms` is now always present, `[]` when empty.

## Test it

```sh
make test       # main_test.go: a local server, no network
echo '{"protocol":"fogping-probe/1","type":"http","host":"example.com","port":443,
       "packets":3,"gap_ms":200,"timeout_ms":3000,"options":{}}' | ./probe
```

`main_test.go` is the model for testing any prober: call `run` with a request,
read the answer, assert on it. It checks the three behaviours that matter: healthy
server, unhealthy server (loss), own failure (error answer).

## Try changing it

- Add an option that sends a header, and think about which characters you must
  refuse so a value cannot inject a second header.
- Time the TLS handshake alone with `net/http/httptrace`, and answer with that.
- Make it check that the body contains a string. What must you bound first?
