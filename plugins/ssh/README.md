# `ssh` — time an SSH server's greeting (Bash)

An SSH server speaks first: the moment you connect it sends one line such as
`SSH-2.0-OpenSSH_9.6`. This prober connects, waits for that line and times it. It
never logs in and sends nothing. (sshd logs "Connection closed [preauth]" per
probe; that is normal.)

Shell is a good language for a quick prober and a dangerous one for careless
code. [`probe`](probe) is written to show the one rule that keeps it safe.

## Use it

```sh
cd .. && ./install.sh ssh
```

Target type SSH, host `server.example`, port `22`.

| Option | Meaning |
|---|---|
| Banner starts with | default `SSH-2.0-`. The first line must start with this, or the probe is lost. |

Needs Bash 5 (`EPOCHREALTIME`) and GNU `timeout`.

## The one rule

> **Never let text from the request become part of a command.**

In a shell, text is code unless you take care. A host named `$(reboot)` or
`a;reboot` contains no space, so fogping accepts it. Spliced into a command it would
run. Here is how the file stays safe, in order:

1. **Parse with `sed`, then validate.** There is no JSON parser in plain Bash, and
   adding `jq` adds a dependency. Because the request has a fixed shape, `sed`
   extracts each field. Whatever it returns is then checked against a whitelist like
   `^[A-Za-z0-9._:-]{1,253}$`. Only a value that passes is ever used.
2. **Quote every expansion.** `"$host"`, never `$host`.
3. **Pass values as arguments, never inside the script text.** The code that runs
   in the child is a *single-quoted constant*:
   ```sh
   bash -c "$PROBE_SCRIPT" _ "$host" "$port" "$read_timeout" "$expect"
   ```
   The values arrive as `$1…$4`. Nothing the user typed can change what the script
   *is*.
4. **Print only things you made.** Error messages are constants from this file;
   numbers are computed here and checked with `[[ $us =~ ^[0-9]{1,12}$ ]]` before
   they go into the JSON.
5. **Match literally.** `[[ $line == "$4"* ]]` — the quotes make the expected
   banner a literal prefix, not a glob pattern.

## Timing

Timing runs **inside** the child with `EPOCHREALTIME`, so starting `bash` is not
counted. The outer `timeout` stops a server that accepts the connection and never
speaks.

## Test it

```sh
./test.sh
```

15 checks against fake servers on localhost, including a healthy server, a wrong
banner, a silent server (bounded wait), nothing listening, and **hostile hosts**
(`$(touch …)`, backticks, `;`) with a check that the injected command never ran.
Writing the attack as a test is how you know the defence works.

## Try changing it

- Add an option for a second banner prefix to accept (`SSH-1.99-`). Which
  characters would you whitelist for it?
- Parse the version out of the banner and refuse old ones. Do it with `[[ =~ ]]`,
  not `eval`.
- Replace `sed` with `jq` and compare. Which is easier to trust, and why?
