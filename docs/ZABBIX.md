# Multi-point monitoring with Zabbix — a complete example

fogping measures from the machine it runs on: one instance, one vantage
point. To watch the same services **from several locations** — the job
SmokePing's master/slave mode does — run one fogping per location and let
**Zabbix** pull them together. There is no master and no slave: no fogping
knows the others exist, nothing is pushed, and there is no pairing to
configure. Zabbix polls each fogping's read-only stats endpoint once a
minute; aggregation, dashboards and alerting are Zabbix's side of the
bargain. fogping itself never alerts.

This page walks through the whole setup for three locations. Everything
in it can be copied as printed.

## The example

Three sites, each running one fogping:

| Site | fogping host | Zabbix host name |
|---|---|---|
| Tokyo (HQ) | `10.0.1.10` | `fogping-tokyo` |
| Osaka (branch) | `10.0.2.10` | `fogping-osaka` |
| Frankfurt (branch) | `10.0.3.10` | `fogping-frankfurt` |

Every fogping measures the same three targets, so the sites can be
compared like for like:

- the company API in Tokyo (TCP, port 443)
- the public web site (TCP, port 443)
- its own site gateway (ICMP)

One Zabbix server (7.0) can reach all three fogping hosts on port 8518.
A Zabbix proxy per site works the same way; nothing below changes.

## Step 1 — at each site: fogping with `--zabbix`

Install fogping as in the [quick start](../README.md#quick-start), add
your three targets with `--edit`, then restart it with the Zabbix switch
on. The switch needs a token — one per site, so a leaked token exposes
one location, not all three. Make it with fogping itself (the file is
created readable by its owner only, and the token is never printed):

```bash
cd ~/fogping
./fogping --gen-token=/etc/fogping/zabbix.token
FOGPING_ZABBIX_TOKEN_FILE=/etc/fogping/zabbix.token ./fogping --zabbix user=admin passwd=change-me
```

Use your own login in place of `user=admin passwd=change-me`, or your
usual way of starting fogping (systemd, Docker — in Docker the same is
`-e FOGPING_ZABBIX=1 -e FOGPING_ZABBIX_TOKEN=<the token>`).

The startup log's security summary now shows `zabbix on: GET /api/stats
with a read-only token`. Without `--zabbix` the endpoint does not exist
at all — a start without the switch is exactly the fogping you had.

## Step 2 — prove it works, before Zabbix is involved

From the Zabbix server (or proxy), ask each fogping for its summary:

```bash
curl -H "Authorization: Bearer <tokyo-token>" http://10.0.1.10:8518/api/stats
```

You should get one line of JSON:

```json
{"window_sec":300,"targets":[{"id":1,"name":"api-tokyo","rounds":5,"p50":3.1,"p90":4.0,"p99":6.2,"loss_pct":0,"bursts":0,"up":1,"probe_errors":0}]}
```

Every target appears with its last-5-minutes median, p90, p99, loss
percentage, loss bursts and up/down state. The three failures are easy
to tell apart — this is the check that replaces "never sure it worked":

| Answer | Meaning |
|---|---|
| `200` with JSON | this site is ready; move on |
| `401` | the token does not match this fogping's token |
| `404` | this fogping was started without `--zabbix` |
| no answer at all | the host or port is unreachable from the Zabbix server |

Repeat for Osaka and Frankfurt with their own tokens.

## Step 3 — in Zabbix: import the template once

*Data collection → Templates → Import*, choose
[`zabbix/template_fogping/7.0/template_fogping.yaml`](../zabbix/template_fogping/7.0/template_fogping.yaml).
Importing is done once for the whole setup, not per site. What the
template contains — items, graphs, triggers — is listed in
[its README](../zabbix/template_fogping/7.0/README.md).

## Step 4 — in Zabbix: one host per location

For each site: *Data collection → Hosts → Create host*, name it as in
the table above (`fogping-tokyo`, …), link the **fogping by HTTP**
template. No agent interface is needed; Zabbix polls over HTTP.

On each host's *Macros* tab set its own two values:

| Macro | Tokyo example |
|---|---|
| `{$FOGPING.URL}` | `http://10.0.1.10:8518` (no trailing slash) |
| `{$FOGPING.TOKEN}` | Tokyo's token from step 1 (type: secret text) |

Three hosts, three URLs, three different tokens. That is the whole
multi-point configuration — there is nothing else to pair or sync.

## Step 5 — see all locations in one place

Within a minute, *Monitoring → Latest data* shows one set of items per
target **per host**: `fogping-tokyo`'s view of the API next to
`fogping-frankfurt`'s. Targets are discovered automatically; adding a
target at a site adds it here at the next poll.

To compare locations on one screen, make a Zabbix dashboard with a
graph widget per question, for example "API p99 by location": one item
each from `fogping-tokyo`, `fogping-osaka` and `fogping-frankfurt`, all
`fogping.p99[<api target id>]`. When the Frankfurt line climbs while
Tokyo stays flat, the problem is Frankfurt's path to the API — not the
API. That comparison is the point of measuring from more than one place.

Alerting is Zabbix's job, per location, with the template's triggers:

- `packet loss over 20% for 5m` (Warning) — per target, per site;
- `loss burst flagged` (Info), suppressed while the loss trigger is
  active, so one dead target raises one problem;
- `no data from {$FOGPING.URL} for 5m` (Warning) — the fogping at that
  site is down, unreachable, or lost its token. The trigger names the
  site, so silence is located, not just noticed.

## What crosses over — and what stays

| In Zabbix (per location, refreshed every minute) | Stays in each fogping |
|---|---|
| p50 / p90 / p99 latency, last 5 minutes | every raw probe sample |
| loss %, loss bursts, up/down per target | the smoke graph itself |
| the template's triggers and your own | target editing, history, export |

The stats endpoint is read-only and returns only target ids, names and
the numbers above — no host names, ports, versions or raw samples. For
the full smoke graph of one target, open that site's fogping; the
Zabbix numbers tell you which site and which target to open.

## Adding a fourth location

Repeat steps 1, 2 and 4 for the new site: install fogping, make *its*
token, start it with `--zabbix`, create its Zabbix host with its two
macros. The template is already imported; the other sites are not
touched, and nothing on them needs to know the new site exists.
