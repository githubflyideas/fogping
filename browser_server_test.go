package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestBrowserServer serves the real router, login and CSP included, on a
// random loopback port for the browser tests in browser/. It is skipped
// unless they start it, and stops when they signal it (or after 5 minutes).
func TestBrowserServer(t *testing.T) {
	if os.Getenv("BROWSER_TEST_SERVE") == "" {
		t.Skip("started by the browser tests in browser/")
	}
	srv := httptest.NewServer(newMux(&Config{}, nil, nil, map[string]string{"admin": "browser-test-password"}))
	defer srv.Close()
	fmt.Println("BROWSER_TEST_URL=" + srv.URL)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	<-ctx.Done()
}
