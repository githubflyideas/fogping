# fogping by HTTP

Zabbix 7.0 template for [fogping](https://github.com/githubflyideas/fogping), a
single-binary latency monitor that keeps every probe sample and charts the
latency *distribution*.

Zabbix's own `icmpping` reports whether a host answers and its average
latency. fogping adds the shape: median, p90 and p99 latency, packet loss, and
loss bursts flagged with a robust z-score. This template brings those numbers
into Zabbix for graphs, correlation and your own triggers. fogping keeps doing
the measuring; Zabbix only reads a summary.

## What you get

For every fogping target, discovered automatically:

| Item | Key | Units |
|---|---|---|
| median latency | `fogping.p50[<id>]` | ms |
| p90 latency | `fogping.p90[<id>]` | ms |
| p99 latency | `fogping.p99[<id>]` | ms |
| packet loss | `fogping.loss[<id>]` | % |
| loss bursts | `fogping.bursts[<id>]` | count |

All values cover the last 5 minutes and are refreshed every minute by a single
HTTP request. Items are keyed by fogping's numeric target id, so renaming a
target in fogping keeps its history.

- **Graph**: `latency p50 / p90 / p99` per target, three lines on one chart.
- **Triggers**:
  - `packet loss over 20% for 5m` (Warning);
  - `loss burst flagged` (Info), suppressed while the loss trigger is active,
    so a dead target raises one problem, not two;
  - `no data from {$FOGPING.URL} for 5m` (Warning), on the template itself:
    fogping unreachable, wrong token, or stats not enabled.

A target with no replies in the window (down, or a slow target with no round
yet) gets no latency values rather than zeros.

## Setup

**1. Turn on fogping's stats API.** It is off by default. Give fogping a token
of at least 16 characters, through the environment or a file, never on the
command line:

```bash
openssl rand -hex 24 > /etc/fogping.stats-token && chmod 600 /etc/fogping.stats-token
FOGPING_STATS_TOKEN_FILE=/etc/fogping.stats-token ./fogping user=admin
# or: FOGPING_STATS_TOKEN=<token> ./fogping ...
```

fogping logs `stats API on` when it starts. Check it from the Zabbix server or
proxy:

```bash
curl -H "Authorization: Bearer $(cat /etc/fogping.stats-token)" http://<fogping-host>:8518/api/stats
```

You should see `{"window_sec":300,"targets":[...]}`. A `401` means the token is
wrong; a `404` means fogping was started without a token.

**2. Import the template.** *Data collection → Templates → Import*, choose
`template_fogping.yaml`.

**3. Create a host and link the template.** *Data collection → Hosts → Create
host*, any name (for example `fogping`), link **fogping by HTTP**. No interface
is needed: the server or proxy polls fogping over HTTP.

**4. Set the two macros** on the host's *Macros* tab:

| Macro | Value |
|---|---|
| `{$FOGPING.URL}` | `http://<fogping-host>:8518`, no trailing slash |
| `{$FOGPING.TOKEN}` | the token from step 1 (secret text) |

**5. Check.** Within a minute, *Monitoring → Latest data* for the host shows
one set of items per fogping target. If not, open *Data collection → Hosts →
Items* and look at `fogping: Get stats` for the error.

## Notes

- The stats API is read-only and returns only target ids, names and the
  numbers above: no hosts, ports, versions or raw samples.
- Wrong tokens are rate-limited per address like wrong passwords.
- fogping itself never alerts. The triggers here are Zabbix's; tune their
  thresholds in the template to taste.
- Adding a target in fogping adds it here at the next poll; a target removed in
  fogping is cleaned up by Zabbix's discovery lifetime.
