package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// The rules every /api endpoint follows are written down in docs/API.md and
// enforced by api_test.go. This file holds the parts they share: the version
// header, the one error shape, and the safety net that puts the router's own
// 404 and 405 answers into that shape.

// apiVersion is the API version this build speaks. Within it, changes are only
// additive; anything else needs /api/v2/.
const apiVersion = "1"

// apiError is the only error body the API sends.
type apiError struct {
	Error string `json:"error"` // for a person
	Code  string `json:"code"`  // for a program; fixed per status, see errorCode
}

// errorCode is the stable machine code for a status. Clients branch on it, so a
// code never changes meaning once published.
func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "unavailable"
	}
	if status >= 500 {
		return "internal"
	}
	return "bad_request"
}

// jsonErr answers with the error shape.
func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Error: msg, Code: errorCode(status)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// apiRules wraps the router. Every /api response says which API version it
// speaks, and an error that is not already JSON — in practice the router's own
// "404 page not found" and "Method Not Allowed" — is rewritten into the error
// shape, keeping its status and headers (Allow, for a 405).
func apiRules(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("API-Version", apiVersion)
		aw := &apiWriter{ResponseWriter: w}
		next.ServeHTTP(aw, r)
		aw.finish()
	})
}

type apiWriter struct {
	http.ResponseWriter
	status int          // set once a header is written (or held)
	held   bool         // a non-JSON error is being held to be rewritten
	body   bytes.Buffer // what the handler wrote while held
}

func (a *apiWriter) WriteHeader(status int) {
	if a.status != 0 {
		return
	}
	a.status = status
	if status >= 400 && !strings.HasPrefix(a.Header().Get("Content-Type"), "application/json") {
		a.held = true
		return
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *apiWriter) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if a.held {
		return a.body.Write(b)
	}
	return a.ResponseWriter.Write(b)
}

func (a *apiWriter) finish() {
	if !a.held {
		return
	}
	msg := strings.TrimSpace(a.body.String())
	switch {
	case a.status == http.StatusNotFound && (msg == "" || msg == "404 page not found"):
		msg = "no such endpoint"
	case a.status == http.StatusMethodNotAllowed && (msg == "" || msg == "Method Not Allowed"):
		msg = "method not allowed"
	case msg == "":
		msg = strings.ToLower(http.StatusText(a.status))
	}
	h := a.ResponseWriter.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json; charset=utf-8")
	a.ResponseWriter.WriteHeader(a.status)
	json.NewEncoder(a.ResponseWriter).Encode(apiError{Error: msg, Code: errorCode(a.status)})
}
