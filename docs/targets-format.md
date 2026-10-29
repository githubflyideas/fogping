# Target list file format (`netping-targets`, version 1)

pingping and fogping read and write the same file, so a list exported from one
imports into the other. This document is identical in both repositories; the
machine-readable version is [`targets.schema.json`](targets.schema.json)
(JSON Schema, draft 2020-12).

## Example

```json
{
  "format": "netping-targets",
  "version": 1,
  "name": "Japan top 3",
  "description": "Large sites, probed with a TCP connect to port 443",
  "targets": [
    {"name": "Google JP", "type": "tcp", "host": "www.google.co.jp", "port": 443, "pace": "normal"},
    {"name": "Gateway", "type": "ping", "host": "10.0.0.1", "pace": "fast", "interval": 10}
  ]
}
```

## The file

UTF-8 JSON, one object, at most **1 MB** and at most **500 targets**.

| Field | Required | Meaning |
|---|---|---|
| `format` | yes | Always `"netping-targets"`. Anything else is refused. |
| `version` | yes | Integer, `1`. A higher version is refused with "newer than this build understands" rather than half-read. |
| `name` | no | Human title of the list, shown in the import preview. |
| `description` | no | Free text. |
| `targets` | yes | Array of target objects (may be empty). |

Fields not listed here are ignored, at the top level and in each target, so a
later version can add optional fields without breaking older readers.

## A target

| Field | Required | Meaning |
|---|---|---|
| `type` | yes | `"ping"` (ICMP echo) or `"tcp"` (time to complete a TCP connect). `"icmp"` is accepted as a synonym for `"ping"` on import; export always writes `"ping"`. Case-insensitive. |
| `host` | yes | Hostname, IPv4 or IPv6 address. No spaces, at most 253 characters. Hostnames are resolved every round. |
| `port` | tcp only | 1-65535. Ignored for `ping`. |
| `name` | no | What the chart shows. At most 64 printable characters, unique within one installation. Empty means the address: `host` for ping, `host:port` for tcp. |
| `pace` | no | `"fast"` (every 15 s), `"normal"` (60 s, the default) or `"slow"` (300 s). |
| `interval` | no | Seconds between rounds, 1-86400, overriding the pace. `0` or absent means "use the pace". |

Every row goes through exactly the validation the web editor uses, so an import
cannot create a target the editor could not.

## Export

`GET /api/targets/export` returns the file as a download named
`pingping-targets-YYYYMMDD.json` or `fogping-targets-YYYYMMDD.json`.

- Only the fields above are written: no ids, history, accounts, settings or
  secrets.
- Only active targets, in the order they were created, so exporting an unchanged
  list twice gives the same `targets` array.
- `pace` is always written; `port` only for tcp; `interval` only when set.

## Import

`POST /api/targets/import` with the file as the request body and
`Content-Type: application/json`. Add `?dry_run=1` for a preview that changes
nothing. Both return:

```json
{
  "name": "Japan top 3",
  "total": 5,
  "add":     [{"row": 1, "name": "Google JP", "type": "tcp", "host": "www.google.co.jp", "port": 443, "pace": "normal"},
              {"row": 2, "name": "Old", "type": "ping", "host": "192.0.2.1", "pace": "normal", "restores_history": true}],
  "skip":    [{"row": 3, "name": "Gateway", "code": "exists", "reason": "a target with this name already exists"},
              {"row": 4, "name": "Google JP", "code": "duplicate", "reason": "same name as row 1 of this file"}],
  "invalid": [{"row": 5, "name": "x", "error": "tcp target needs a port 1-65535"}],
  "dry_run": true,
  "imported": 0
}
```

Rows are numbered from 1 in file order.

- **Add-only.** A row whose name belongs to an active target is skipped
  (`exists`); existing targets are never changed or removed. A name repeated
  inside the file is imported once; later rows are skipped (`duplicate`).
- **Deleted names.** If the name belongs to a deleted target, importing it does
  what adding it in the editor does: the old row is reactivated with the file's
  settings and its history comes back. The preview marks such rows with
  `"restores_history": true`.
- **Invalid rows** are listed with the reason and left out; they do not block the
  valid ones.
- **All or nothing.** The rows in `add` are written in one database transaction
  and the prober reloads once. If anything fails (for example another session
  added one of the names between preview and confirm), nothing is applied and the
  request answers `409`; preview again.
- The confirmation is evaluated afresh against the list as it is at that moment;
  the dry run is advice, not a reservation.

Whole-file errors (wrong `format`, newer `version`, no `targets` array, not
JSON) answer `400`; more than 500 targets or more than 1 MB answers `413`.

**Importing makes this host probe every address in the file.** Read the preview
before confirming, and only import lists from sources you trust.

## Who may export and import

| | pingping | fogping |
|---|---|---|
| Export | any signed-in account (admin or viewer) — the same accounts that can already read the target list | whoever can read the target list: everyone when no `user=` is configured, otherwise a signed-in user; also on a read-only instance |
| Import | admin accounts only | only when started with `--edit` (and signed in, when users are configured) |

Import also takes the same cross-site protection as the other write endpoints: a
request whose `Origin` is a different site is refused, and the JSON content type
means a page on another site cannot submit it with your cookie.

## Ready-made lists

Ready-made lists: see `presets/` (coming).
