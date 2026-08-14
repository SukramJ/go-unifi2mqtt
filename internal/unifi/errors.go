// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package unifi

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors callers match with errors.Is.
var (
	// ErrUnauthorized reports a rejected API key or an expired classic
	// session. On the Integration API it is terminal — the key is wrong
	// and retrying cannot help, so the caller stops the loop rather than
	// hammering the console (CONCEPT.md §8.4).
	ErrUnauthorized = errors.New("unifi: unauthorized")

	// ErrForbidden reports an authenticated caller lacking permission,
	// which usually means the API key was created by a read-only admin
	// while a control was invoked.
	ErrForbidden = errors.New("unifi: forbidden")

	// ErrNotFound reports a missing object or, on a startup probe, an
	// endpoint the console's Network version does not have.
	ErrNotFound = errors.New("unifi: not found")

	// ErrRateLimited reports a 429. The transport already honoured
	// Retry-After before giving up, so seeing this means the console is
	// still limiting after the advertised wait.
	ErrRateLimited = errors.New("unifi: rate limited")

	// ErrCapabilityUnavailable reports a call that needs the classic API
	// layer while it is disabled or unhealthy. The coordinator queries
	// Facade.Has before building discovery so this stays rare, but a
	// capability can also disappear at runtime when the classic login
	// starts failing.
	ErrCapabilityUnavailable = errors.New("unifi: capability unavailable")
)

// APIError is a non-2xx response from the console.
//
// It carries the classic API's `meta.msg` alongside the HTTP status
// because that field is often the only thing distinguishing two
// failures that share a status code (`api.err.LoginRequired` vs
// `api.err.NoPermission`, both 401).
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Method and Path locate the call. The path never carries the API
	// key — it travels in a header — so this is safe to log.
	Method string
	Path   string
	// Code is the machine-readable error identifier: `meta.msg` on the
	// classic API, the `statusName` field on the Integration API. Empty
	// when the response carried no parseable body.
	Code string
	// Message is the human-readable detail, when the response had one.
	Message string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	s := fmt.Sprintf("unifi: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap maps the status onto a sentinel so callers can branch with
// errors.Is instead of comparing status codes at every call site.
func (e *APIError) Unwrap() error {
	switch e.Status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		return nil
	}
}

// Retryable reports whether repeating the request could plausibly
// succeed. 5xx and 429 are transient; a 4xx means the request itself is
// wrong and will stay wrong.
func (e *APIError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}
