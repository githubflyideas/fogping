# HTTP API

fogping's web UI and the Zabbix template talk to fogping through this API. The
same rules hold in pingping and vrf-ping, so a client written for one reads the
others the same way.

## Rules

These are enforced by tests (`api_test.go`): code that breaks one does not pass CI.

1. **Version.** This is API version 1. Every `/api/` response carries the header
   `API-Version: 1`. Within version 1, changes are only additive: new endpoints,
   new optional request fields, new response fields. Removing or renaming a field,
   or changing what it means, needs a new prefix `/api/v2/`, and `/api/` keeps
   answering as before.
2. **Methods.** Every endpoint names its methods. Reads are `GET`, creates
   `POST`, replacements `PUT`, deletions `DELETE`, actions `POST`. Any other
   method gets `405` with an `Allow` header.
3. **JSON in, JSON out.** A request with a body sends
   `Content-Type: application/json` (anything else gets `415`). Every response,
   errors included, is `application/json; charset=utf-8`, except the targets
   export, which is a JSON file sent as a download.
4. **One error shape.** Every error is
   `{"error": "<message for a person>", "code": "<code for a program>"}`.
   Programs branch on `code` (or the status); people read `error`. The codes:

   | Status | `code` |
   |---|---|
   | 400 | `bad_request` |
   | 401 | `unauthorized` |
   | 403 | `forbidden` |
   | 404 | `not_found` |
   | 405 | `method_not_allowed` |
   | 409 | `conflict` |
   | 413 | `too_large` |
   | 415 | `unsupported_media_type` |
   | 429 | `rate_limited` (with `Retry-After`) |
   | 500 | `internal` |
   | 503 | `unavailable` |

   An unknown path under `/api/` is a `404` in the same shape.
5. **Names.** JSON keys are `snake_case`, in requests and responses. Times are
   Unix seconds; a duration or size says its unit in its name (`interval_sec`,
   `window_sec`, `retention_days`); a percentage ends in `_pct`. The one
   exception is the compact sample format of `/api/series` (below), which predates
   these rules and is kept for compatibility.
6. **Authentication.** The web UI signs in with `POST /api/login` and then sends
   the `fogping_session` cookie. Machine clients use a bearer token
   (`Authorization: Bearer …`) where an endpoint says so. A missing or bad
   credential is `401`. Writes also refuse a cross-origin browser request (`403`).
7. **Documented.** Every endpoint is listed below. A test compares this table with
   the routes the code registers, both ways.

## Endpoints

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | /api/login | none | Sign in. Body `{"user": "…", "pass": "…"}`; sets the session cookie. Repeated failures from one address get `429`. |
| POST | /api/logout | none | Sign out: the session ends on the server too. Safe to call without one. |
| GET | /api/version | none / session | `{"version"}` for everyone; with a session (or no login configured) also `retention_days`, `hot_days`, `editable`. |
| GET | /api/probe-types | session | The probe types this fogping can run, in display order: `[{"type", "label", "uses_port"}]`. The UI builds its type filter and form from it. |
| GET | /api/targets | session | Every target with its last-hour and last-day summary. |
| POST | /api/targets | session, `--edit` | Create a target. Unknown fields are refused. |
| PUT | /api/targets/{id} | session, `--edit` | Replace a target. |
| DELETE | /api/targets/{id} | session, `--edit` | Delete a target (its history is kept). |
| GET | /api/targets/export | session | The target list as a `netping-targets` file ([format](targets-format.md)). |
| POST | /api/targets/import | session, `--edit` | Import a `netping-targets` file. `?dry_run=1` reports without applying. |
| GET | /api/series | session | Rounds for one target: `?target=NAME` and either `?minutes=N` or `?from=&to=` (Unix seconds). |
| GET | /api/stats | bearer token | Per-target summary of the last 5 minutes, for Zabbix. Exists only with `--zabbix` ([template](../zabbix/template_fogping/7.0/README.md)). |

"session" means: a signed-in session when fogping was started with a login,
anyone otherwise.

## `/api/series` format

```json
{"tier": "raw", "rounds": [{"t": 1700000000, "s": 20, "r": 19, "ms": [12.1, 12.4], "b": true, "z": 3.8}]}
{"tier": "hourly", "buckets": [{"t": 1700000000, "q": [10.2, 12.0, 14.1, 19.5, 30.0], "loss": 1.5, "n": 60, "bursts": 1}]}
```

Up to a day the answer is `raw`: one entry per round — `t` time (Unix seconds),
`s` packets sent, `r` received, `ms` every reply's round-trip time in
milliseconds, `b` true when the round was flagged as a loss burst, `z` the robust
z-score behind that flag. Longer windows are `hourly`: one bucket per hour —
`q` min/p50/p90/p99/max in ms (absent when every packet was lost), `loss`
percent of packets lost, `n` rounds in the bucket, `bursts` flagged rounds.

A round that could not be measured at all (DNS failed, no ICMP socket) has `s: 0`
and `e` set to the reason. It is not loss: it is left out of every statistic, and
`/api/targets` and `/api/stats` count such rounds apart as `probe_errors`.
