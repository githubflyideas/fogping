# Prober options

A prober is one way of measuring a target. Every target has the same core fields
(`name`, `host`, `port`, `pace`, `interval_sec`); anything else a prober needs —
a DNS record type, a TTL, a greeting — is an **option**. This page is the
contract between fogping and the people who write probers.

## Declare, don't build

A prober lists its options when it registers. fogping then provides the rest:

| You write | fogping does |
|---|---|
| the option spec | the editor form, validation, defaults, storage, export, import, the API |
| a round function that reads `t.OptString("record")` | passes the stored, already-validated values |

```go
registerProber(&prober{
    name: "dns", label: "DNS",
    options: []optionSpec{
        {Key: "record", Label: "Record type", Kind: optChoice, Choices: []string{"A", "AAAA", "MX"}, Default: "A"},
        {Key: "query",  Label: "Name to look up", Kind: optString, Required: true},
        {Key: "retries", Label: "Retries", Kind: optInt, Min: 0, Max: 5, Default: 1},
        {Key: "use_tcp", Label: "Use TCP", Kind: optBool},
    },
    validate: func(t *TargetCfg) error { return nil },
    round:    func(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) { ... },
})
```

## The spec

| Field | Meaning |
|---|---|
| `Key` | lowercase `snake_case`, at most 32 characters, unique within the prober |
| `Label` | what the editor shows |
| `Help` | tooltip text (optional) |
| `Kind` | `string`, `int`, `bool` or `choice` |
| `Required` | the target cannot be saved without it (unless it has a `Default`) |
| `Default` | used when the value is missing or empty; must itself be valid |
| `Min`, `Max` | `int`: inclusive range (`Max` 0 = no upper bound) |
| `MaxLen` | `string`: at most this many characters (default 256) |
| `Choices` | `choice`: the allowed values |

A malformed spec (bad key, duplicate, unusable default) panics when fogping
starts, so it is caught the first time the prober is run, never by a user.

## Rules

- **Flat.** Options are one object: keys to string, whole number, boolean, or one
  of a list. Nothing nested. At most 20 per prober and 4 KB per target.
- **Strict.** Unknown keys are an error naming the options that exist, so a typo
  is never a silently ignored setting.
- **Validated once.** The round function receives values that have already passed
  the spec. It reads them with `OptString`, `OptInt`, `OptBool`.
- **Additive.** Options never change what a core field means. After release, an
  option is not renamed or retyped; new ones must be optional or have a default.
  That keeps stored targets and exported files working across versions.
- **Plain text storage.** Options are stored and exported as written. Do not
  make a prober that needs a password as an option; secrets get their own
  mechanism before any prober needs one.

## Where options appear

- API: `GET /api/probe-types` lists each type's option specs; targets carry an
  `options` object in `GET/POST/PUT /api/targets`.
- Targets file: an `options` object on the target row. Readers that do not know
  the field ignore it, as the [format](targets-format.md) says of any unknown field.
