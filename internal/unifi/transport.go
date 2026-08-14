// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package unifi is the shared HTTP layer both UniFi API clients sit on:
// TLS configuration, timeouts, retry with backoff, rate-limit handling
// and redaction on the way to the log.
//
// The subpackages hold the two API flavours — integration (official,
// API-key authenticated) and classic (unofficial, cookie session). This
// package additionally provides the Facade that combines them, so
// nothing above it needs to know which surface answered a call
// (CONCEPT.md §3.3).
package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxErrorBodyBytes bounds how much of a failed response is read for
// the error message. A console returning an HTML error page would
// otherwise land megabytes of markup in a log line.
const maxErrorBodyBytes = 8 << 10

// Doer is the subset of *http.Client the transport uses, so tests can
// inject a stub without spinning up a server.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// TransportConfig configures a [Transport].
type TransportConfig struct {
	// BaseURL is the console origin, e.g. "https://192.168.1.1:443".
	BaseURL string
	// Timeout bounds a single request attempt.
	Timeout time.Duration
	// Retries is the number of additional attempts after a transient
	// failure. Zero means one attempt total.
	Retries int
	// VerifyTLS enables certificate verification.
	VerifyTLS bool
	// CAFile is an optional PEM bundle trusted in addition to the system
	// roots. Only consulted when VerifyTLS is true — trusting an extra
	// root while skipping verification entirely would be meaningless.
	CAFile string
	// Logger receives retry and rate-limit diagnostics.
	Logger *slog.Logger
	// Client overrides the HTTP client. Tests inject one here; leaving
	// it nil builds a client from the TLS settings above.
	Client Doer
}

// Transport performs HTTP requests against a UniFi console.
//
// It is safe for concurrent use: the bounded worker pool that fetches
// per-device statistics shares one Transport across goroutines.
type Transport struct {
	base    string
	retries int
	client  Doer
	log     *slog.Logger
}

// NewTransport builds a Transport. It returns an error only for a
// configuration problem it cannot recover from, such as an unreadable
// or malformed CA bundle — silently falling back to the system roots
// there would turn an operator's explicit trust decision into a
// surprise.
func NewTransport(cfg TransportConfig) (*Transport, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	client := cfg.Client
	if client == nil {
		tlsCfg, err := buildTLSConfig(cfg.VerifyTLS, cfg.CAFile)
		if err != nil {
			return nil, err
		}
		client = &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				TLSClientConfig:   tlsCfg,
				ForceAttemptHTTP2: true,
			},
		}
	}

	return &Transport{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		retries: max(cfg.Retries, 0),
		client:  client,
		log:     log,
	}, nil
}

// buildTLSConfig assembles the console's TLS settings.
//
// UniFi consoles serve a self-signed certificate on their LAN address,
// so operators overwhelmingly run with verification off. That is an
// explicit, warned-about choice (see config.Config.Warnings); CAFile is
// the documented way to get verification back by trusting the console's
// own certificate.
func buildTLSConfig(verify bool, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		//nolint:gosec // G402: deliberate, operator-controlled opt-out for
		// self-signed console certificates; defaults to verification on.
		InsecureSkipVerify: !verify,
	}
	if !verify || caFile == "" {
		return cfg, nil
	}

	pem, err := os.ReadFile(caFile) //nolint:gosec // operator-supplied path is the point
	if err != nil {
		return nil, fmt.Errorf("unifi: read CA_FILE %s: %w", caFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("unifi: CA_FILE %s contains no usable certificate", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// Request describes one call.
type Request struct {
	Method string
	// Path is appended to the base URL, e.g.
	// "/proxy/network/integration/v1/sites".
	Path string
	// Query is optional; nil sends no query string.
	Query url.Values
	// Body is marshalled as JSON when non-nil.
	Body any
	// Header carries per-request headers such as X-API-KEY.
	Header http.Header
	// Out receives the decoded JSON response when non-nil. A nil Out
	// discards the body, which is what actuator calls want.
	Out any
}

// Do performs req, retrying transient failures with exponential backoff
// and honouring Retry-After on a 429.
//
// The context governs the whole call including waits between attempts,
// so a shutdown signal aborts a backoff sleep instead of running it out.
func (t *Transport) Do(ctx context.Context, req Request) error {
	backoff := time.Second

	for attempt := 0; ; attempt++ {
		err := t.attempt(ctx, req)
		if err == nil {
			return nil
		}
		if attempt >= t.retries || !retryable(err) {
			return err
		}

		wait := backoffFor(err, backoff)
		t.log.Warn("unifi.retry",
			slog.String("method", req.Method),
			slog.String("path", req.Path),
			slog.Int("attempt", attempt+1),
			slog.Duration("retry_in", wait),
			slog.String("err", err.Error()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

// retryable reports whether err is worth another attempt. A cancelled
// context never is — the caller is shutting down.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// Anything else that got this far is a network-level failure:
	// connection refused, TLS handshake, read timeout. Those are exactly
	// what a console rebooting after a firmware update looks like.
	return true
}

// backoffFor picks the wait before the next attempt. A rate limit
// carries the console's own answer in Retry-After, which always beats
// our guess; everything else uses exponential backoff with jitter so a
// fleet of restarted daemons does not resynchronise onto the console.
func backoffFor(err error, backoff time.Duration) time.Duration {
	var apiErr *retryAfterError
	if errors.As(err, &apiErr) && apiErr.after > 0 {
		return apiErr.after
	}
	jitter := time.Duration(rand.Int64N(int64(backoff / 4))) //nolint:gosec // jitter, not cryptography
	return backoff + jitter
}

// retryAfterError carries a server-advertised wait alongside the
// underlying API error.
type retryAfterError struct {
	*APIError
	after time.Duration
}

func (e *retryAfterError) Unwrap() error { return e.APIError }

func (t *Transport) attempt(ctx context.Context, req Request) error {
	httpReq, err := t.build(ctx, req)
	if err != nil {
		return err
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		// A *url.Error stringifies the full URL including any query, so
		// strip it down to the underlying cause before it reaches a log.
		return fmt.Errorf("unifi: %s %s: %w", req.Method, req.Path, unwrapURLError(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return t.responseError(req, resp)
	}
	if req.Out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(req.Out); err != nil {
		return fmt.Errorf("unifi: %s %s: decode response: %w", req.Method, req.Path, err)
	}
	return nil
}

func (t *Transport) build(ctx context.Context, req Request) (*http.Request, error) {
	u := t.base + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}

	var body io.Reader
	if req.Body != nil {
		buf, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("unifi: encode request body: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("unifi: build request: %w", unwrapURLError(err))
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Accept", "application/json")
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil
}

// responseError turns a non-2xx response into an [APIError], pulling
// out whichever error shape the two API flavours use.
func (t *Transport) responseError(req Request, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))

	apiErr := &APIError{
		Status: resp.StatusCode,
		Method: req.Method,
		Path:   req.Path,
	}
	apiErr.Code, apiErr.Message = parseErrorBody(raw)

	if resp.StatusCode == http.StatusTooManyRequests {
		return &retryAfterError{APIError: apiErr, after: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return apiErr
}

// errorBody covers both flavours: the Integration API returns
// {"statusName": "...", "message": "..."}, the classic API
// {"meta": {"rc": "error", "msg": "api.err.…"}}.
type errorBody struct {
	StatusName string `json:"statusName"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	Meta       struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
}

func parseErrorBody(raw []byte) (code, message string) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", ""
	}
	var b errorBody
	if err := json.Unmarshal(raw, &b); err != nil {
		// Not JSON — an HTML error page or a proxy banner. Keep a short
		// excerpt so the operator sees *something* actionable.
		return "", truncate(strings.TrimSpace(string(raw)), 200)
	}
	switch {
	case b.Meta.Msg != "":
		return b.Meta.Msg, ""
	case b.StatusName != "":
		return b.StatusName, b.Message
	case b.Error != "":
		return b.Error, b.Message
	default:
		return "", b.Message
	}
}

// parseRetryAfter reads the header in both its forms: delta-seconds and
// an HTTP-date. An unparseable value yields zero, which makes the
// caller fall back to normal backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// unwrapURLError strips the *url.Error wrapper, whose Error() embeds
// the full request URL. Nothing secret travels in a UniFi URL today,
// but keeping URLs out of error strings on principle means a future
// query parameter cannot quietly start leaking.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
