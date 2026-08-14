// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web serves the optional diagnostic UI.
//
// It answers one question: is the bridge doing what it should? What the
// console reports, what reached the broker, which poll last succeeded,
// and what failed. It is deliberately read-only — the write path is
// MQTT, and a second one would be a second thing to secure.
//
// The static assets are embedded, so the binary stays self-contained
// and the Home Assistant add-on needs no extra files. All asset and API
// references are relative, which is what lets the same page work
// unchanged behind the Ingress proxy's path prefix.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/state"
	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

//go:embed static
var assets embed.FS

// Config configures the [Server].
type Config struct {
	// Bind is the listen address, "host:port".
	Bind string
	// User and Password enable HTTP basic auth. Both empty disables it,
	// which is only safe on a loopback bind or behind Ingress.
	User     string
	Password string
	// Language selects the UI language ("en" or "de").
	Language string
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger
}

// Server serves the diagnostic UI.
type Server struct {
	cfg   Config
	store *state.Store
	log   *slog.Logger
	mux   *http.ServeMux
}

// New builds a Server over the given store.
func New(cfg Config, store *state.Store) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, store: store, log: log, mux: http.NewServeMux()}
	s.mux.Handle("GET /", http.FileServerFS(sub))
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	return s, nil
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Bind,
		Handler: s.auth(s.mux),
		// Without these a single stalled client can hold a connection
		// open indefinitely; the UI has no long-polling to protect.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("web.listening",
			slog.String("bind", s.cfg.Bind),
			slog.Bool("auth", s.authEnabled()))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Bounded so a hung client cannot delay a systemd stop.
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(stopCtx) //nolint:contextcheck // ctx is already cancelled here
		return ctx.Err()
	}
}

func (s *Server) authEnabled() bool {
	return s.cfg.User != "" || s.cfg.Password != ""
}

// auth wraps the mux in HTTP basic auth when credentials are set.
//
// Comparison is constant-time: a naive == leaks the password one byte
// at a time to anyone who can measure response latency.
func (s *Server) auth(next http.Handler) http.Handler {
	if !s.authEnabled() {
		return next
	}
	wantUser := []byte(s.cfg.User)
	wantPass := []byte(s.cfg.Password)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), wantPass) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="go-unifi2mqtt", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealth is a liveness probe for container orchestration. It
// deliberately reports only that the process serves, not whether the
// console is reachable — a restart would not fix an unreachable
// console, so failing here would produce a crash loop instead.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": "ok", "version": version.Version})
}

// handleState returns the full snapshot the UI renders.
func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.render(s.store.Snapshot()))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The UI polls; a cached response would show stale data with no
	// indication that it is stale.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}
