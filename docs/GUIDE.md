# fogping 使用指南 / Guide

[← 返回 README](../README.md)

## 目录

- [Docker](#docker)
- [系统要求](#requirements)
- [安装](#install)
- [免 root 放行 ICMP](#allow-icmp-ping-without-root)
- [首次启动](#first-start)
- [常用命令](#common-commands)
- [管理监控目标](#managing-targets)
- [作为 systemd 服务运行](#run-as-a-systemd-service)
- [反向代理](#behind-a-reverse-proxy)
- [数据、保留期与备份](#data-retention-and-backup)
- [怎么看图](#reading-the-chart)
- [升级](#upgrading)
- [故障排查](#troubleshooting)
- [从源码构建](#build-from-source)
- [设计说明](#design-notes)

## Docker

Images for linux/amd64, arm64 and arm/v7 (Raspberry Pi, NAS, RouterOS). Data lives in `/data`.

```bash
# first run: --edit so you can add targets in the web UI
docker run -d --name fogping --restart unless-stopped   -p 8518:8518   -v fogping_data:/data   githubflyideas/fogping:latest   --edit user=admin passwd=change-me

# done editing: recreate without --edit (data in ~/fogping is kept)
docker rm -f fogping
docker run -d --name fogping --restart unless-stopped   -p 8518:8518   -v fogping_data:/data   githubflyideas/fogping:latest            user=admin passwd=change-me
```
Everything after the image name is passed to fogping as-is (`--edit`, `--days=30`,
`user=` / `passwd=`). Where you can set environment variables but not a command
(Docker Desktop, NAS container managers, RouterOS), use these instead:

| Variable | Same as |
|---|---|
| `FOGPING_EDIT=1` | `--edit` |
| `FOGPING_USER=admin` / `FOGPING_PASSWD=change-me` | `user=admin passwd=change-me` (comma-separated for several) |
| `FOGPING_DAYS=30` | `--days=30` (default and maximum 93) |

`--edit` without a login is refused, as on bare metal. PING targets work without
extra capabilities on Docker 20.10+; `--user` keeps the mounted directory writable by you.


## Requirements

- Linux on x86-64. The release binary is statically linked, so the distribution
  and its glibc version do not matter.
- For PING targets: permission to send ICMP (see [below](#allow-icmp-ping-without-root)).
  TCP targets need no special permission.
- TCP port 8518 reachable from your browser — or `--localhost` plus a reverse proxy.
  The port is fixed at 8518.
- Nothing else: no database server, no runtime, no web server.

## Install

```bash
sudo mkdir -p /opt/fogping && sudo chown "$USER": /opt/fogping && cd /opt/fogping
wget https://github.com/githubflyideas/fogping/releases/download/v1.1.3/fogping-v1.1.3-linux-amd64.tar.gz
wget https://github.com/githubflyideas/fogping/releases/download/v1.1.3/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS     # expect: fogping-v1.1.3-linux-amd64.tar.gz: OK
tar -xzf fogping-v1.1.3-linux-amd64.tar.gz
./fogping --version
```

The tarball contains only `fogping` and `LICENSE`. Everything fogping writes goes
into one directory it creates where you start it: `data/` — so the start
directory must be writable by the user fogping runs as.
(The systemd section below hands it to a dedicated user.)

## Allow ICMP (ping) without root

fogping first tries an unprivileged ICMP socket and falls back to a raw socket.
Either one needs a permission. Pick one:

**A. Unprivileged ICMP (recommended).** Many distributions already enable it; check:

```bash
sysctl net.ipv4.ping_group_range
# "0 2147483647" -> already allowed for everyone, nothing to do
# "1 0"          -> disabled; enable it permanently:
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-fogping.conf
sudo sysctl --system
```

**B. Give the binary `cap_net_raw`.** Repeat after every upgrade, because a new
binary does not inherit the capability:

```bash
sudo setcap cap_net_raw+ep /opt/fogping/fogping
```

**C. Run as root.** Works, but not needed.

Without any of these, PING targets show 100% loss and every round logs
`cannot open an ICMP socket (needs net.ipv4.ping_group_range or cap_net_raw, see README)`.

## First start

Always start fogping from its install directory: `data/` is relative to the
current working directory.

```bash
cd /opt/fogping
./fogping --edit user=admin passwd=change-me
```

On a brand-new install it creates `data/fogping.db`, adds the demo target and
starts probing. The log looks like this:

```
new database — seeded a demo target (www.google.com)
[Demo] probing www.google.com
fogping 1.1.2 up · 1 targets · targets editable in the web UI · listening on 0.0.0.0:8518 · data in ./data · 93-day retention
➜  open http://localhost:8518 for the smoke graph
web login enabled for 1 user(s)
```

Open `http://<server>:8518` and log in. The first round is sent immediately;
after that every target is probed on its own schedule
(see [pace](#pace-and-interval)). Stop with Ctrl-C — pending data is flushed
before exit.

## Common commands

Everything below is also printed by `./fogping --help`, ready to copy.

Try it out — the web UI is open to anyone who can reach port 8518 on any of the
host's addresses:

```bash
./fogping
```

Require a login (recommended). Logins last 7 days; sessions live in memory, so a
restart logs everyone out:

```bash
./fogping user=admin passwd=change-me
```

Several users — names and passwords are comma-separated and paired by position
(`alice` logs in with `alice-pw`, `bob` with `bob-pw`):

```bash
./fogping user=alice,bob passwd=alice-pw,bob-pw
```

Add, edit or delete targets — start with `--edit`, change them in the web UI,
then press Ctrl-C and start again without `--edit` so the UI is read-only:

```bash
./fogping --edit user=admin passwd=change-me
```

`--edit` refuses to start without a login (or `--localhost`): an open, writable UI
would let anyone make this host ping or connect to arbitrary addresses.

History is kept for 93 days by default, which is also the maximum. Keep less with
`--days` (raw samples are always kept for 2 days; the UI hides range buttons longer
than the history). A value above 93 is refused at startup with `--days max is 93`:

```bash
./fogping --days=30 user=admin passwd=change-me
```

Run in the background with a log file, and stop it again (for a proper service,
see [systemd](#run-as-a-systemd-service)):

```bash
nohup ./fogping user=admin passwd=change-me >> fogping.log 2>&1 &
pkill -x fogping
```

Listen on `127.0.0.1:8518` only, when a reverse proxy in front handles access
(see [reverse proxy](#behind-a-reverse-proxy)):

```bash
./fogping --localhost
./fogping --localhost --edit
```

Print the version:

```bash
./fogping --version
```

Arguments can come in any order, and the port is always 8518. Quote passwords
that contain shell characters: `passwd='p@ss;word'`.

Anything on the command line — including the password — is visible to other
local users (`ps`, `systemctl show`). If that matters on your host, use
`--localhost` and let the reverse proxy handle authentication.

## Managing targets

Targets are stored in the SQLite database and changed in one place only: the web
UI, when fogping runs with `--edit`. There is no config file and no list file.
Without `--edit` nothing — not the UI, not the API — can change a target.

### In the web UI (`--edit`)

1. Start with `--edit` (and a login): `./fogping --edit user=admin passwd=change-me`
2. Click **✎ targets** in the top right.
3. Fill in the form and press **+ add**. For example:

   - PING, host `1.1.1.1`, name `Cloudflare`, pace `fast` — pinged every 15 s,
     30 packets per round.
   - TCP, host `api.example.com`, port `443` — measures how long a TCP connect
     takes; shown as `api.example.com:443` since no name was given.
   - PING, host `10.0.0.1`, name `Gateway`, interval `10` — every 10 s, overriding
     the pace.

   PING targets are IPv4; hostnames are resolved every round. The name is what the
   chart buttons show: optional, up to 64 characters, unique.

4. **edit** changes a target in place; **delete** stops probing it.

Changes take effect immediately, without a restart. When you are done, restart
without `--edit` to make the UI read-only again.

- **Deleting keeps history.** The data stays until retention ages it out. Adding a
  target with the same name again brings its history back.
- **Renaming keeps history.** Renaming onto the name of a deleted target is refused,
  so two histories are never merged by accident.

### Pace and interval

| Pace | Probed every | Packets per round |
|---|---|---|
| `fast` | 15 s | 30 |
| `normal` | 60 s | 20 |
| `slow` | 300 s | 20 |

Packets in a round are sent 50 ms apart; replies later than 1 s count as lost.
A custom interval changes how often a round runs, not how many packets it sends.

### Export and import

The target editor (`--edit`) has **⇩ export** and **⇧ import…** buttons. On a
read-only instance an **⇩ targets** button in the header still exports.

- **Export** downloads the active targets as `fogping-targets-YYYYMMDD.json`
  (`GET /api/targets/export`): name, type, host, port, pace and interval only — no
  ids, history, logins or settings. It needs the same access as viewing the
  targets: a login when `user=` is set, otherwise none.
- **Import** reads such a file (pick it, or drop it on the panel) and first shows
  a preview: rows to add, rows skipped because the name already exists (or repeats
  in the file), and invalid rows with the reason. **Import N targets** applies the
  valid rows in one transaction — all of them or none. It only ever adds: existing
  targets are not changed or removed. A row named like a *deleted* target brings
  that target back with its history, exactly as re-adding it by hand does, and the
  preview says so. Import needs `--edit` (and a login when users are configured)
  and is refused from another site.
- The file format is shared with [pingping](https://github.com/githubflyideas/pingping),
  so lists move between the two in both directions.

```json
{
  "format": "netping-targets",
  "version": 1,
  "name": "Japan top 3",
  "targets": [
    {"name": "Google JP", "type": "tcp", "host": "www.google.co.jp", "port": 443, "pace": "normal"},
    {"name": "Gateway", "type": "ping", "host": "10.0.0.1", "pace": "fast", "interval": 10}
  ]
}
```

`type` is `ping` or `tcp` (`icmp` is accepted), `port` is for tcp only, `pace` is
`fast` / `normal` / `slow` (default `normal`), `interval` (seconds, 1-86400) is
optional. Files are limited to 1 MB and 500 targets. Full spec:
[targets-format.md](targets-format.md) · JSON Schema:
[targets.schema.json](targets.schema.json).

> Importing makes this host probe every address in the file. Read the preview,
> and only import lists you trust.

Ready-made lists: see presets/ (coming).

## Run as a systemd service

Create a dedicated user and hand it the install directory:

```bash
sudo useradd --system --home-dir /opt/fogping --shell /usr/sbin/nologin fogping
sudo chown -R fogping: /opt/fogping
```

`/etc/systemd/system/fogping.service`:

```ini
[Unit]
Description=fogping link-quality probe
After=network-online.target
Wants=network-online.target

[Service]
User=fogping
WorkingDirectory=/opt/fogping
ExecStart=/opt/fogping/fogping user=admin passwd=change-me
# Only if you chose option B for ICMP and not ping_group_range:
# AmbientCapabilities=CAP_NET_RAW
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
sudo chmod 600 /etc/systemd/system/fogping.service   # it contains the password
sudo systemctl daemon-reload
sudo systemctl enable --now fogping
journalctl -u fogping -f                             # watch the log
```

To edit targets on a running service, stop it, run once in the foreground with
`--edit` as the same user, then start it again:

```bash
sudo systemctl stop fogping
cd /opt/fogping && sudo -u fogping ./fogping --edit user=admin passwd=change-me
# ... edit in the browser, then Ctrl-C
sudo systemctl start fogping
```

If you are not using a reverse proxy, open the port:
`sudo ufw allow 8518/tcp` or `sudo firewall-cmd --add-port=8518/tcp --permanent && sudo firewall-cmd --reload`.

## Behind a reverse proxy

Run fogping with `--localhost` and let the proxy provide TLS (and, if you like,
authentication instead of fogping's own login).

Caddy:

```
fogping.example.com {
    reverse_proxy 127.0.0.1:8518
}
```

nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8518;
    proxy_set_header Host $host;   # required for --edit, see below
}
```

The target editor refuses requests whose `Origin` does not match the `Host` they
arrive with (cross-site request protection). Caddy passes the original `Host`
through by default; nginx does not unless you set `proxy_set_header Host $host`.
Without it, saving a target fails with `cross-origin request refused`.

## Data, retention and backup

Everything lives in `data/fogping.db` (plus `-wal` and `-shm` files while it runs):
targets, raw rounds and hourly buckets.

| Tier | What | Kept | Used for views |
|---|---|---|---|
| Raw | every sample of every round | 2 days | up to 1 day |
| Hourly | per hour: min / p50 / p90 / p99 / max, loss, bursts | `--days` (93, max 93) | 3 days and longer |

Hourly buckets are computed in-process: on start for everything raw still holds,
then every 5 minutes. Old data is removed every night at 00:05 local time, and the
file is compacted when there is free space to hand back. There is nothing to cron.

Size, roughly: ~0.35 MB per `normal` target and ~2 MB per `fast` target for the
two days of raw samples, plus ~1.8 KB per target per day of hourly history
(~170 KB per target for the full 93 days). The raw tier dominates: 200 `normal`
targets come to about 105 MB, 200 `fast` targets to about 430 MB.

Upgrading from a version that kept more than 93 days: hourly rows older than the
new retention are deleted by the next nightly clean-up (00:05). Back up `data/`
first if you want to keep them.

Backup — while running, with the `sqlite3` tool:

```bash
sqlite3 /opt/fogping/data/fogping.db ".backup /backup/fogping-$(date +%F).db"
```

or stop the service and copy the whole `data/` directory. Do not copy
`fogping.db` alone while fogping is running.

To start over from scratch: stop fogping and delete `data/`.

## Reading the chart

- **Line** — the median RTT. **Smoke** — the spread around it: on views up to
  1 day every individual sample, beyond that each hour's min/p50/p90/p99/max.
  Tight smoke is a steady link; tall smoke is jitter.
- **Red bars** — packet loss (right axis, %).
- **◆** — a loss burst: loss far above the target's own recent baseline
  (robust z-score ≥ 3.5). There are no thresholds to configure.
- **P50 / P90 / P99** under the chart are computed from all samples in the window.
  On views of 3 days and longer they are marked `≈`: exact window percentiles
  cannot be rebuilt from hourly ones, so these are the typical hour's values.
- **Compare** — the COMPARE menu overlays an earlier period's median as a dashed
  line: *Previous period* (the window just before this one), *Yesterday, same
  time* or *Last week, same time*. Hover shows the compare value and the Δ%.
  A choice is greyed out ("Beyond retention") when that period is older than the
  93-day history, so 60d and longer windows cannot compare with the previous period.
  When the period has no data yet the legend says so; a compare line far above
  the main chart is clipped at the top with its peak shown (`↑ N ms`).
- For an exact range, fill in the two date fields and press **Go**;
  **✕ selection** returns to the preset windows.

## Upgrading

```bash
cd /opt/fogping
sudo systemctl stop fogping
sudo cp -a data data.bak                 # optional, lets you roll back
sudo wget https://github.com/githubflyideas/fogping/releases/download/vX.Y.Z/fogping-vX.Y.Z-linux-amd64.tar.gz
sudo tar -xzf fogping-vX.Y.Z-linux-amd64.tar.gz    # replaces the binary only
sudo setcap cap_net_raw+ep fogping       # only if you used ICMP option B
sudo systemctl start fogping
```

Schema changes are applied automatically on start.

**From 1.1.0:** the `targets/` directory and its `*.imported` files are no longer
used; delete them.

**From 1.0.x:** `ping.list` and `tcp.list` are no longer read. Either upgrade to
v1.1.0 first and start it once (it imports the lists into the database), then
upgrade to the latest release; or start the new version with `--edit` and add the
targets again in the web UI using **the same names** — history is keyed by name,
so it comes back. The old daily table is dropped (hourly data covers the same span).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| PING targets at 100% loss, log shows `cannot open an ICMP socket` | ICMP permission missing — see [Allow ICMP](#allow-icmp-ping-without-root). |
| `--edit needs a login or --localhost` | By design: add a login, or bind to localhost behind a proxy. |
| Saving a target says `read-only: restart fogping with --edit` | fogping was started without `--edit`. |
| Saving a target says `cross-origin request refused` | Your proxy does not forward `Host` — see [reverse proxy](#behind-a-reverse-proxy). |
| `bind: address already in use` | Another fogping is already running (e.g. the service, while you start `--edit` by hand). |
| Everyone was logged out | fogping restarted; sessions are kept in memory. |
| `data/` appeared in an unexpected place | fogping was started from another directory; `cd` into the install directory or set `WorkingDirectory=`. |
| After upgrading from 1.0.x there are no targets | List files are no longer read — see [Upgrading](#upgrading). |
| `store init failed: ... permission denied` | The start directory is not writable by the user running fogping — `chown` it. |

## Build from source

Requires Go 1.22+ and a C compiler (SQLite is linked through cgo):

```bash
git clone https://github.com/githubflyideas/fogping.git && cd fogping
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o fogping .
go test ./...
```

Only linux/amd64 is released: cgo rules out cross-compiling the other platforms
from the release runner. A local build links glibc dynamically; the release
tarball is linked statically.

## Design notes


fogping is a minimalist smokeping-like network oscilloscope with SQLite storage:
raw rounds are held hot for 2 days, then rolled up hourly and kept for 93 days
(`--days` to shorten; 93 is the maximum). Burst detection (z-score) writes its verdict alongside each
round, so the ◆ marks on the chart come straight from the store. Built-in Web UI
with native auth — read-only unless started with `--edit` — no Nginx, no Caddy, no
external database. Targets are rows in the same SQLite file; there is no config file.

Sibling project: [pingping](https://github.com/githubflyideas/pingping) — the same
oscilloscope without cgo.
