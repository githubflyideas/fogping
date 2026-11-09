package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
)

// Security headers.
//
// Every response says it must not be framed (X-Frame-Options plus CSP
// frame-ancestors) and must not be MIME-sniffed. The read-only UI is meant to be
// shared as a link, not embedded in someone else's page, so nothing here needs
// framing.
//
// HTML pages get a full Content-Security-Policy on top. Their inline <script>
// blocks are allowed by SHA-256 hash, computed from the embedded file itself, so
// editing a page never leaves a stale hash behind. Inline styles stay allowed:
// the pages use style="" attributes and ECharts writes inline styles.

const frameOnlyCSP = "frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", frameOnlyCSP)
		next.ServeHTTP(w, r)
	})
}

// inlineScriptRe matches attribute-less <script> blocks: the only inline form the
// pages use. TestPageScriptsAllCovered keeps it that way.
var inlineScriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

func inlineScriptHashes(page []byte) []string {
	var out []string
	for _, m := range inlineScriptRe.FindAllSubmatch(page, -1) {
		sum := sha256.Sum256(m[1])
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// pageCSP is the policy for one HTML page.
func pageCSP(page []byte) string {
	script := append([]string{"'self'"}, inlineScriptHashes(page)...)
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(script, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		frameOnlyCSP,
	}, "; ")
}
