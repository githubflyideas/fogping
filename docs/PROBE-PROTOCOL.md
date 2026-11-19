# Probe protocol `fogping-probe/1` — declaring a prober

This is the standard every fogping prober follows, whatever it is written in: Go
compiled into fogping, or Python, Ruby, a shell script or Rust run as a separate
program. Everything here is language-neutral JSON. The Go code in fogping is one
implementation of it, not the definition of it.

This document covers **what a prober declares and how its settings are
validated**. How fogping starts a prober and exchanges rounds with it is the next
part of the protocol and is not in this version of the document.

Three files define the standard:

| File | What it is |
|---|---|
| this document | the rules, in words |
| [`probe-manifest.schema.json`](probe-manifest.schema.json) | the manifest's shape, as JSON Schema |
| [`conformance/`](conformance) | test vectors: input and the exact expected result |

If the words and the vectors ever disagree, the vectors are right and the words
are a bug.

## 1. The manifest

A prober declares itself with one JSON object:

```json
{
  "protocol": "fogping-probe/1",
  "type": "dns",
  "label": "DNS",
  "uses_port": false,
  "options": [
    {"key": "query",   "label": "Name to look up", "kind": "string", "required": true, "max_len": 253},
    {"key": "record",  "label": "Record type",     "kind": "choice", "choices": ["A", "AAAA", "MX"], "default": "A"},
    {"key": "retries", "label": "Retries",         "kind": "int", "min": 0, "max": 5, "default": 1},
    {"key": "use_tcp", "label": "Use TCP",         "kind": "bool"}
  ]
}
```

| Field | Rule |
|---|---|
| `protocol` | exactly `fogping-probe/1` |
| `type` | lowercase letters, digits, `-`, `_`; starts with a letter; at most 32 characters. The value targets carry in their `type` field. |
| `label` | 1-16 characters; what the UI shows |
| `uses_port` | the target has a meaningful `port` (default false) |
| `options` | at most 20 option specs (default none) |

Fields not listed here are ignored, so version 1 can gain optional fields
without breaking older readers.

### Option specs

| Field | Rule |
|---|---|
| `key` | lowercase letters, digits, `_`; starts with a letter; at most 32 characters; unique within the prober |
| `label` | non-empty; what the form shows |
| `help` | optional tooltip text |
| `kind` | `string`, `int`, `bool` or `choice` |
| `required` | the target cannot be saved without a value (unless there is a `default`) |
| `default` | used when the value is missing; must itself pass the spec |
| `min`, `max` | `int`: inclusive range. `min` defaults to 0. `max` 0 or absent = no upper bound |
| `max_len` | `string`: at most this many characters (default 256) |
| `choices` | `choice`: the allowed values, at least one |

## 2. Validating settings

A user's settings for a target arrive as one flat JSON object. Every
implementation turns it into the canonical object, or an error, by these rules,
in this order:

1. A key the spec does not declare is an error `unknown_option`.
2. For each declared option, in declaration order:
   - A string is trimmed. A value that is absent, `null` or an empty string is
     **missing**.
   - A missing value takes the `default` if there is one; otherwise it is an
     error `required` if the option is required; otherwise it is left out.
   - A present value is checked by `kind`:
     - `string`: must be text (`wrong_type`), no control characters
       (`bad_value`), at most `max_len` characters (`too_long`).
     - `choice`: must be text (`wrong_type`) equal to one of `choices`,
       case-sensitively (`bad_choice`).
     - `int`: must be a JSON number with no fractional part (`wrong_type`; text
       such as `"2"` is not accepted) within `min`..`max` (`out_of_range`).
     - `bool`: must be `true` or `false` (`wrong_type`). `false` and `0` are
       values, not missing.
3. The canonical object holds only the options that have a value. Its encoded
   size is at most 4096 bytes (`too_large`).

The first error found is the one reported.

## 3. Error codes

Programs branch on the code; people read the message. Wording is not part of the
standard, codes are.

| Code | Meaning |
|---|---|
| `bad_protocol` | not a manifest object, or `protocol` is not `fogping-probe/1` |
| `bad_type` | `type` breaks its rule |
| `bad_label` | a `label` is empty or too long |
| `bad_key` | an option `key` breaks its rule |
| `duplicate_key` | two options share a key |
| `bad_kind` | `kind` is not one of the four |
| `missing_choices` | a `choice` option has no `choices` |
| `bad_default` | a `default` fails its own spec |
| `too_many_options` | more than 20 options |
| `unknown_option` | settings contain a key the spec does not declare |
| `required` | a required option has no value |
| `wrong_type` | the value is the wrong JSON type |
| `out_of_range` | an `int` outside `min`..`max` |
| `bad_choice` | not one of `choices` |
| `too_long` | a string longer than `max_len` |
| `bad_value` | a string with control characters |
| `too_large` | manifest over 64 KB, or settings over 4096 bytes |

## 4. Staying compatible

- Within `fogping-probe/1` the standard only grows: new optional manifest fields,
  new optional option fields, new error codes for new checks. A change that makes
  an accepted manifest or settings object invalid needs `fogping-probe/2`.
- Once a prober has shipped, its options are not renamed or retyped, and a new
  option is optional or has a default. Stored targets and exported target files
  keep working across versions.
- Settings are stored and exported as written, in plain text. A prober that needs
  a password has to wait for the secrets mechanism; do not put one in an option.

## 5. Proving your implementation

An implementation in any language is conformant when it gives the expected
answer for every vector:

- [`conformance/manifest.json`](conformance/manifest.json): each entry is a
  manifest and either no `error` (must be accepted) or the code to report.
- [`conformance/options.json`](conformance/options.json): each entry is a set of
  option specs, a user's `input`, and either the exact canonical `want` or the
  `error` code. Compare as JSON, so `2` and `2.0` are the same number.

fogping runs both files in its own tests (`conformance_test.go`); a library for
another language should run them the same way. To add a rule, add vectors first.

## 6. In Go

Inside fogping a prober registers with `registerProber` and its `options` are the
same specs, Go struct tags matching the JSON above. [OPTIONS.md](OPTIONS.md) is
the short guide for that route.
