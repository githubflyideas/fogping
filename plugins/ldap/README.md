# `ldap` — time an LDAP directory (Go)

An LDAP server (OpenLDAP, Active Directory, 389) is often what every login depends
on, so "is it answering, and how fast?" is worth a chart. This prober connects the
way a client would and times how far it gets.

Read [`main.go`](main.go) top to bottom: it is the reference for writing a prober in
Go and for the structure every prober follows. (fogping's own HTTP check,
[`httpprobe.go`](../../httpprobe.go), is the same idea built in; read it for the
other way to add a type to fogping.)

## Use it

```sh
cd .. && ./install.sh ldap       # make builds ./probe; the LDAP client library is vendored by go.sum
./fogping --probes --edit --localhost
```

Target type LDAP, host `ldap.example.com`, port `389` (or `636` for ldaps).

| Option | Meaning |
|---|---|
| Security | `none` (389), `starttls` (389, upgraded), `ldaps` (636, TLS from the first byte). Certificates are always verified. |
| Measure until | `connect`: TCP and TLS. `bind`: also an anonymous bind. `rootdse` (default): also reading the root DSE. |

**No password, on purpose.** Options are stored as plain text, so this prober never
asks for credentials. Everything it does works anonymously. The *root DSE* is a
small public entry every LDAP server publishes at the empty name `""`, listing its
naming contexts and supported versions. Reading it proves the directory accepts and
answers queries, not merely that the port is open. A server that refuses anonymous
reads makes that probe lost: measure `bind` or `connect` instead.

## How it works

1. `run` reads the request (bounded to 64 KB) and hands it to `parse`.
2. `parse` validates everything, including rules only this prober knows. The host is
   checked against a whitelist (`validHost`) because we build a network address
   from it, and fogping only refuses spaces.
3. `probeOnce` is one probe, and its structure is the lesson:
   - **One deadline for the whole probe.** `remaining()` gives each step whatever
     time is left, so a slow connect cannot be followed by a full timeout for every
     later step.
   - **Stages.** It returns as soon as it reaches `stage`, so one prober measures
     three different things.
   - **Closing is not timed.** `defer conn.Close()` sends the polite "unbind" after
     the clock has been read.
   - **TLS is verified.** There is no "skip certificate check" option. Go checks the
     certificate against the host name, or against its IP entries when the target is
     an address.
4. Anything the server or network is responsible for — refusing, a failed bind, an
   empty root DSE, silence, a bad certificate — is an error from `probeOnce`, and
   `measure` turns it into a **lost probe**: no entry in `ms`.
5. Only a bad request (a hostile host, port 0, an unknown stage) is an **error
   answer**. The two answer types, `measurement` and `failure`, make mixing them up
   impossible to write, and `ms` is always present, `[]` when empty.

## Test it

```sh
make test
```

`main_test.go` writes a fake LDAP server in about 40 lines with the same BER library
the client uses: the messages are `SEQUENCE { messageID, [APPLICATION n] … }`, and
the fake answers bind (0), search (3) and unbind (2). That is how you test a binary
protocol: speak just enough of it. The tests check that

- every stage works against a healthy server;
- a refused bind, a missing root DSE and a server that accepts and never answers
  are all *loss* (and the silent one really waits, then stops at the deadline);
- a self-signed certificate on `ldaps` is *rejected*: a prober that accepted it would
  report healthy a directory that real clients refuse;
- bad requests are error answers with no measurement fields.

## Try changing it

- Add a `base_dn` option and search it instead of the root DSE. A search filter built
  from user text is an injection risk in LDAP just as in SQL: read RFC 4515 and
  `ldap.EscapeFilter` before you let a user influence one.
- Add an authenticated bind. Where would the password come from, given that options
  are plain text? (This is why fogping does not offer one yet.)
- Report the root DSE's `supportedLDAPVersion` in the error text when a probe fails.
