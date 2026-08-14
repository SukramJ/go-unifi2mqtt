// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package unifi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTransport(t *testing.T, baseURL string, retries int) *unifi.Transport {
	t.Helper()

	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
		Retries: retries,
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tr
}

func TestDoDecodesJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Accept"), "application/json"; got != want {
			t.Errorf("Accept = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("limit"); got != "200" {
			t.Errorf("limit = %q, want 200", got)
		}
		_, _ = io.WriteString(w, `{"name":"ok","count":3}`)
	}))
	t.Cleanup(srv.Close)

	var out struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	err := newTransport(t, srv.URL, 0).Do(t.Context(), unifi.Request{
		Method: http.MethodGet,
		Path:   "/thing",
		Query:  map[string][]string{"limit": {"200"}},
		Out:    &out,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Name != "ok" || out.Count != 3 {
		t.Errorf("decoded %+v, want {ok 3}", out)
	}
}

// A nil Out discards the body, which is what actuator calls want — they
// care about the status code, not the payload.
func TestDoWithoutOutDiscardsBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got, want := string(body), `{"action":"RESTART"}`; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
		if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}
		_, _ = io.WriteString(w, `{"ignored":true}`)
	}))
	t.Cleanup(srv.Close)

	err := newTransport(t, srv.URL, 0).Do(t.Context(), unifi.Request{
		Method: http.MethodPost,
		Path:   "/actions",
		Body:   map[string]string{"action": "RESTART"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	var out map[string]bool
	err := newTransport(t, srv.URL, 3).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x", Out: &out,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
}

// A 4xx means the request itself is wrong — repeating it just wastes
// the console's time and delays the real error.
func TestDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	err := newTransport(t, srv.URL, 5).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x",
	})
	if !errors.Is(err, unifi.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1 — a 401 must not be retried", got)
	}
}

func TestRetriesExhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := newTransport(t, srv.URL, 2).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x",
	})
	if err == nil {
		t.Fatal("Do succeeded against a permanently failing console")
	}
	// Retries is "attempts after the first", so 2 retries means 3 calls.
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
}

// The console's own Retry-After beats our exponential guess: it knows
// when its limiter resets and we do not.
func TestHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	var (
		attempts atomic.Int32
		first    time.Time
		second   time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch attempts.Add(1) {
		case 1:
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			second = time.Now()
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)

	var out map[string]bool
	err := newTransport(t, srv.URL, 2).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x", Out: &out,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	waited := second.Sub(first)
	// The default first backoff is 1s + up to 250ms jitter, so a plain
	// backoff would be indistinguishable from a 1s Retry-After here.
	// What this asserts is that the header is at least respected as a
	// floor rather than ignored.
	if waited < 900*time.Millisecond {
		t.Errorf("waited %v before retrying, want at least the advertised 1s", waited)
	}
}

// An unparseable Retry-After must fall back to normal backoff rather
// than being treated as "retry immediately" or "wait forever".
func TestBadRetryAfterFallsBackToBackoff(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "next tuesday")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	var out map[string]bool
	if err := newTransport(t, srv.URL, 2).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x", Out: &out,
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("made %d attempts, want 2", got)
	}
}

func TestRateLimitErrorSurfacesAfterRetries(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	err := newTransport(t, srv.URL, 1).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x",
	})
	if !errors.Is(err, unifi.ErrRateLimited) {
		t.Errorf("error = %v, want ErrRateLimited", err)
	}
}

// Cancelling must abort the wait between attempts, not run it out —
// otherwise a shutdown stalls for the full backoff.
func TestContextCancelsDuringBackoff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := newTransport(t, srv.URL, 10).Do(ctx, unifi.Request{
		Method: http.MethodGet, Path: "/x",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	// Ten retries at 1s+ each would be well over a second.
	if elapsed > time.Second {
		t.Errorf("took %v to abort, want the cancellation to cut the backoff short", elapsed)
	}
}

func TestErrorBodyParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "integration API shape",
			body:        `{"statusName":"NOT_FOUND","message":"device not found"}`,
			wantCode:    "NOT_FOUND",
			wantMessage: "device not found",
		},
		{
			// The classic API puts its machine-readable code in meta.msg,
			// which is often the only thing separating two failures that
			// share a status code.
			name:     "classic API shape",
			body:     `{"meta":{"rc":"error","msg":"api.err.LoginRequired"},"data":[]}`,
			wantCode: "api.err.LoginRequired",
		},
		{
			// A reverse proxy in front of the console answers with HTML;
			// keeping an excerpt beats logging an empty error.
			name:        "html error page",
			body:        `<html><body>502 Bad Gateway</body></html>`,
			wantMessage: "<html><body>502 Bad Gateway</body></html>",
		},
		{
			name: "empty body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)

			err := newTransport(t, srv.URL, 0).Do(t.Context(), unifi.Request{
				Method: http.MethodGet, Path: "/x",
			})

			var apiErr *unifi.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not an *APIError: %v", err)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, tt.wantCode)
			}
			if apiErr.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, tt.wantMessage)
			}
			if !strings.Contains(apiErr.Error(), "400") {
				t.Errorf("Error() = %q, want it to name the status", apiErr.Error())
			}
		})
	}
}

// A giant HTML error page must not land in a log line in full.
func TestErrorBodyIsTruncated(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", 100_000))
	}))
	t.Cleanup(srv.Close)

	err := newTransport(t, srv.URL, 0).Do(t.Context(), unifi.Request{
		Method: http.MethodGet, Path: "/x",
	})
	if err == nil {
		t.Fatal("Do succeeded")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("error message is %d bytes, want it truncated", len(err.Error()))
	}
}

func TestAPIErrorSentinelMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   error
		retry  bool
	}{
		{http.StatusUnauthorized, unifi.ErrUnauthorized, false},
		{http.StatusForbidden, unifi.ErrForbidden, false},
		{http.StatusNotFound, unifi.ErrNotFound, false},
		{http.StatusTooManyRequests, unifi.ErrRateLimited, true},
		{http.StatusInternalServerError, nil, true},
		{http.StatusBadRequest, nil, false},
	}

	for _, tt := range tests {
		e := &unifi.APIError{Status: tt.status, Method: "GET", Path: "/x"}
		if tt.want != nil && !errors.Is(e, tt.want) {
			t.Errorf("status %d does not map to %v", tt.status, tt.want)
		}
		if got := e.Retryable(); got != tt.retry {
			t.Errorf("status %d Retryable() = %v, want %v", tt.status, got, tt.retry)
		}
	}
}

// The console's URL should not appear in transport errors: nothing
// secret travels there today, but keeping URLs out on principle means a
// future query parameter cannot quietly start leaking into logs.
func TestNetworkErrorsDropTheURL(t *testing.T) {
	t.Parallel()

	// A port nothing listens on produces a connection-refused error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	err := newTransport(t, url, 0).Do(t.Context(), unifi.Request{
		Method: http.MethodGet,
		Path:   "/secret-path",
		Query:  map[string][]string{"token": {"do-not-log-me"}},
	})
	if err == nil {
		t.Fatal("Do succeeded against a closed server")
	}
	if strings.Contains(err.Error(), "do-not-log-me") {
		t.Errorf("error leaked a query parameter: %v", err)
	}
}

func TestCAFileErrors(t *testing.T) {
	t.Parallel()

	// A CA file the operator named but that cannot be used must fail
	// loudly: silently falling back to the system roots would turn an
	// explicit trust decision into a surprise.
	_, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: "https://example.invalid", VerifyTLS: true,
		CAFile: filepath.Join(t.TempDir(), "missing.pem"),
	})
	if err == nil {
		t.Error("NewTransport accepted a missing CA file")
	}

	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: "https://example.invalid", VerifyTLS: true, CAFile: bad,
	}); err == nil {
		t.Error("NewTransport accepted a malformed CA file")
	}

	// With verification off the CA file is meaningless, so naming a
	// missing one must not block startup.
	if _, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: "https://example.invalid", VerifyTLS: false,
		CAFile: filepath.Join(t.TempDir(), "missing.pem"),
	}); err != nil {
		t.Errorf("NewTransport failed with verification off: %v", err)
	}
}

// UniFi consoles serve self-signed certificates on their LAN address,
// so the daemon has to be able to talk to one.
func TestSelfSignedConsoleWorksWithVerifyOff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	var out map[string]bool
	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: srv.URL, Timeout: 5 * time.Second, VerifyTLS: false, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr.Do(t.Context(), unifi.Request{Method: http.MethodGet, Path: "/x", Out: &out}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	// ...and with verification on, the same console must be rejected.
	tr, err = unifi.NewTransport(unifi.TransportConfig{
		BaseURL: srv.URL, Timeout: 5 * time.Second, VerifyTLS: true, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr.Do(t.Context(), unifi.Request{Method: http.MethodGet, Path: "/x", Out: &out}); err == nil {
		t.Error("a self-signed certificate was accepted with VerifyTLS on")
	}
}
