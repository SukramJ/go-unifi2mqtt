// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// logCapture is a slog.Handler that records rendered records so tests
// can assert that something was reported rather than silently dropped.
//
// Enabled answers true for every level on purpose — a capture that
// dropped debug records could not tell "not logged" from "logged too
// quietly". The consequence is that a capture-based assertion sees a
// demoted record exactly as it saw the original, so a test that cares
// about an operator actually reading the line has to check the level
// itself. That is what [capturedRecord.level] and [logCapture.find] are
// for: the daemon's own default level is Info (see main.go), so a line
// demoted to Debug is invisible in production and identical here.
type logCapture struct {
	mu      sync.Mutex
	lines   []string
	records []capturedRecord
}

// capturedRecord is one record, with the level a rendered line loses.
type capturedRecord struct {
	level slog.Level
	msg   string
	text  string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, b.String())
	c.records = append(c.records, capturedRecord{level: r.Level, msg: r.Message, text: b.String()})
	return nil
}

// find returns the last record logged under msg.
func (c *logCapture) find(msg string) (capturedRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.records) - 1; i >= 0; i-- {
		if c.records[i].msg == msg {
			return c.records[i], true
		}
	}
	return capturedRecord{}, false
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
