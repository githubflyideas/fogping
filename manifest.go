package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The probe manifest: how any prober, in any language, tells fogping what it is.
// The standard is docs/PROBE-PROTOCOL.md and docs/probe-manifest.schema.json;
// this file is the reference implementation of its validation rules, checked by
// the shared vectors in docs/conformance.

const (
	probeProtocol    = "fogping-probe/1"
	maxManifestBytes = 64 << 10
)

var manifestTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Manifest is the declaration a prober sends. Fields fogping does not know are
// ignored, so version 1 can gain optional fields without breaking older readers.
type Manifest struct {
	Protocol string       `json:"protocol"`
	Type     string       `json:"type"`
	Label    string       `json:"label"`
	UsesPort bool         `json:"uses_port"`
	Options  []optionSpec `json:"options"`
}

// parseManifest decodes and validates a manifest. The error carries one of the
// standard codes (errCode).
func parseManifest(raw []byte) (*Manifest, error) {
	if len(raw) > maxManifestBytes {
		return nil, coded(codeTooLarge, "manifest is larger than %d bytes", maxManifestBytes)
	}
	if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '{' {
		return nil, coded(codeBadProtocol, "manifest is not a JSON object")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, coded(codeBadProtocol, "manifest is not valid: %v", err)
	}
	switch {
	case m.Protocol != probeProtocol:
		return nil, coded(codeBadProtocol, "protocol must be %q, got %q", probeProtocol, m.Protocol)
	case !manifestTypeRe.MatchString(m.Type):
		return nil, coded(codeBadType, "type %q must be lowercase letters, digits, - or _, starting with a letter, at most 32 characters", m.Type)
	case strings.TrimSpace(m.Label) == "" || utf8.RuneCountInString(m.Label) > 16:
		return nil, coded(codeBadLabel, "label must be 1-16 characters")
	}
	if err := validateSpecs(m.Options); err != nil {
		return nil, err
	}
	return &m, nil
}
