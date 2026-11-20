package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"unicode"
)

// What a prober returns for one round, judged as untrusted input: a prober may be
// someone else's program, so nothing in its answer is taken on trust. The rules
// are in docs/PROBE-PROTOCOL.md (section 7) and the vectors in
// docs/conformance/round.json.

const (
	maxResultBytes = 64 << 10
	maxRTTms       = 600_000 // ten minutes: nothing a round could legitimately measure is longer
	maxErrorChars  = 200
)

// Round result error codes.
const (
	codeBadResult   = "bad_result"  // not one JSON object, wrong shape, or over the size cap
	codeBadCounts   = "bad_counts"  // sent/received out of range or inconsistent
	codeBadSamples  = "bad_samples" // ms entries missing, not numbers, negative, NaN, too large
	codeBadErrorMsg = "bad_error"   // error text empty
	codeAmbiguous   = "ambiguous"   // both a measurement and an error, or neither
)

type roundResult struct {
	Protocol string     `json:"protocol"`
	Sent     *int       `json:"sent"`
	Received *int       `json:"received"`
	MS       *[]float64 `json:"ms"`
	Error    *string    `json:"error"`
}

// parseRoundResult validates one prober answer for a round of `packets` probes
// started at unix time t, and returns the Round fogping stores. A prober's own
// failure ({"error": ...}) is not an invalid answer: it becomes a Round with S=0
// and E set, the same as a built-in probe failure.
func parseRoundResult(raw []byte, t int64, packets int) (Round, error) {
	var r Round
	if len(raw) > maxResultBytes {
		return r, coded(codeBadResult, "result is larger than %d bytes", maxResultBytes)
	}
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '{' {
		return r, coded(codeBadResult, "result is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var res roundResult
	if err := dec.Decode(&res); err != nil {
		return r, coded(codeBadResult, "result is not valid: %v", err)
	}
	if dec.More() {
		return r, coded(codeBadResult, "more than one JSON value")
	}
	if res.Protocol != probeProtocol {
		return r, coded(codeBadProtocol, "protocol must be %q", probeProtocol)
	}
	measured := res.Sent != nil || res.Received != nil || res.MS != nil
	if measured == (res.Error != nil) {
		return r, coded(codeAmbiguous, "answer with either sent, received and ms, or error, not both and not neither")
	}
	r.T = t
	if res.Error != nil {
		msg := strings.Join(strings.FieldsFunc(*res.Error, unicode.IsControl), " ")
		msg = strings.Join(strings.Fields(msg), " ")
		if msg == "" {
			return r, coded(codeBadErrorMsg, "error text is empty")
		}
		if rs := []rune(msg); len(rs) > maxErrorChars {
			msg = string(rs[:maxErrorChars])
		}
		r.E = msg
		return r, nil
	}
	if res.Sent == nil || res.Received == nil || res.MS == nil {
		return r, coded(codeBadResult, "sent, received and ms are all required")
	}
	s, rc, ms := *res.Sent, *res.Received, *res.MS
	switch {
	case s < 1 || s > packets:
		return r, coded(codeBadCounts, "sent must be 1-%d", packets)
	case rc < 0 || rc > s:
		return r, coded(codeBadCounts, "received must be 0-%d", s)
	case len(ms) != rc:
		return r, coded(codeBadSamples, "ms has %d entries for %d received", len(ms), rc)
	}
	for _, v := range ms {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > maxRTTms {
			return r, coded(codeBadSamples, "ms entries must be numbers from 0 to %d", maxRTTms)
		}
	}
	r.S, r.R = s, rc
	r.MS = append([]float64{}, ms...)
	return r, nil
}
