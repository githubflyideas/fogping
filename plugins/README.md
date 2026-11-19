# Official probers — and a course in writing your own

fogping measures a target with a **prober**. ICMP ping and TCP connect are built
in. Everything else is a small program, in any language, that fogping runs once per
round. (HTTP is built in too: see [`httpprobe.go`](../httpprobe.go), the same idea
compiled into fogping, pingping and vrf-ping.) This directory holds five official
ones. They are real: you can run them against
production, and each one is also written to be read, with the comments saying
*why*, not just *what*.

| Prober | Measures | Language | What reading it teaches |
|---|---|---|---|
| [`ldap`](ldap/README.md) | an LDAP directory: connect, bind, root DSE, with TLS | **Go** | the protocol end to end; testing a binary protocol with a fake server; one deadline for a multi-step probe |
| [`dns`](dns/README.md) | latency of a DNS server | **Python** | a binary wire format built by hand; ignoring forged replies; one deadline per probe |
| [`smtp`](smtp/README.md) | mail server greeting and EHLO, with TLS | **Ruby** | a line protocol with multi-line replies; never reading without a bound; verifying certificates |
| [`ssh`](ssh/README.md) | time for an SSH server to greet | **Bash** | the one rule of shell: input never becomes code |
| [`traceroute`](traceroute/README.md) | latency to the destination or to one router on the path | **Rust** | system calls without root; `unsafe` done carefully; making invalid input unrepresentable |

## Trust

A prober runs with fogping's own privileges, once per round. Install only probers
you trust, and run fogping as an ordinary account, not as root. fogping refuses to
load from a directory that others can write to; see section 8 of
[docs/PROBE-PROTOCOL.md](../docs/PROBE-PROTOCOL.md).

## Use them

```sh
plugins/install.sh ldap dns smtp ssh traceroute     # or: plugins/install.sh all
./fogping --probes --edit --localhost               # probes are off unless you say so
```

`install.sh` copies each prober into `./probes/<name>/`. Open the editor, pick the
new type in the type menu, and fill in its own options. The form is built from the
prober's manifest, so there is nothing to configure in fogping itself.

Needs: Go for `ldap`, Python 3 for `dns`, Ruby for `smtp`, Bash 5 and GNU
coreutils for `ssh`, Rust (cargo) for `traceroute` (Linux). Only the compiled ones
are built at install time.

## What a prober is

A directory with two files:

```
probes/dns/
  manifest.json   what the prober is called and which options it takes
  probe           an executable: a script with a #! line, or a compiled binary
```

For every round fogping runs `probe` **once**, and the whole conversation is:

1. fogping writes **one JSON object** on the prober's standard input:
   ```json
   {"protocol":"fogping-probe/1","type":"dns","host":"1.1.1.1","port":53,
    "packets":10,"gap_ms":200,"timeout_ms":2000,"options":{"query":"example.com"}}
   ```
2. the prober measures `packets` probes, `gap_ms` apart, waiting at most
   `timeout_ms` for each;
3. it writes **one JSON object** on standard output:
   ```json
   {"protocol":"fogping-probe/1","sent":10,"received":9,"ms":[12.1,12.4,11.9,12.0,13.2,12.2,12.5,12.3,12.8]}
   ```
   `received` is how many probes were answered; `ms` lists the round-trip time of
   each of those. The ones that were not answered simply are not in the list. That
   is how packet loss is expressed;
4. it exits.

If the prober cannot measure at all (bad option, no permission) it answers
`{"protocol":"fogping-probe/1","error":"why"}` instead. fogping records such a
round as *could not be measured*, which is not packet loss and is left out of every
statistic. **Lost probes are loss; a broken prober is an error. Keep them apart.**

The full contract, the exact rules fogping applies to what a prober answers, and
test vectors for every rule: [docs/PROBE-PROTOCOL.md](../docs/PROBE-PROTOCOL.md).

## The six rules every prober here follows

Every file in this directory demonstrates these. They are the difference between a
prober that works and one you can leave running.

1. **Input is a request, not a command.** Nothing is on the command line, and the
   environment is nearly empty. A value from the request is data: never `eval` it,
   never splice it into a shell string, SQL or URL without checking it first.
2. **Check against what is allowed, not what is dangerous.** A hostname is letters,
   digits, `.`, `-`, `_`, `:`. Say that, and refuse everything else.
3. **Bound everything.** Read at most 64 KB of request, a bounded number of
   bytes and lines from a server, and use one deadline per probe so that stray
   packets cannot stretch it.
4. **Lost probe or broken prober?** A server that does not answer, answers wrongly
   or has a bad certificate is *loss*. A bad option, a missing permission or an
   unresolvable server is an *error answer*.
5. **Always all three: `sent`, `received`, `ms`.** `ms` is `[]` when everything was
   lost, never absent and never `null`. `len(ms)` equals `received`.
6. **Never turn off verification to make a test pass.** TLS certificates are
   checked; DNS replies are matched by ID and source; HTTP statuses are judged.

## Writing your own

1. Pick a `type` (lowercase letters, digits, `-`, `_`) and make `probes/<type>/`.
2. Write `manifest.json`: label, whether the target has a port, and your options.
   Each option has a key, a label, a kind (`string`, `int`, `bool`, `choice`), and
   optional default, range or choices. fogping validates them before your prober
   sees them, and builds the editor form from them.
3. Write `probe`. Start from the prober closest to what you need, and keep its
   structure: read request, validate, measure, answer.
4. Run it by hand: `echo '{...}' | ./probe`. Each README here shows how.
5. Test it against a fake server, as every prober here does. Then install it and
   run fogping with `--probes`.
6. Check your manifest and answers against the shared vectors in
   [docs/conformance/](../docs/conformance), in the language of your choice.

Prober options are plain text. Do not ask for a password in one.
