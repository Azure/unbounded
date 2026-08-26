// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package log is the structured-logging entry point for Gantry.
//
// The design docs mandate WARN-level emission in specific places (forced
// cache eviction in the design doc, HRW rank mismatch in the design doc). This package wraps
// log/slog with a consistent attribute vocabulary so those WARN lines are
// uniformly tagged and machine-parseable.
//
// Standard attributes:
//
//	subsystem one of {"mirror","transfer","cache","origin","coord",
//	 "discovery","hrw","members","cdsub","agent"}
//	digest OCI digest string ("sha256:...")
//	peer NodeID of a remote peer
//	registry upstream registry name
//	repo OCI repository
//	class the design doc failure class
//
// Level conventions:
//
//	DEBUG per-RPC traces, per-byte transfer milestones
//	INFO state transitions, lifecycle events
//	WARN the design doc forced eviction, the design doc HRW rank mismatch, soft failures
//	 that the design explicitly calls out
//	ERROR hard failures requiring operator attention
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a *slog.Logger configured for the given level and format.
// format is "json" or "text"; anything else is treated as "json" with a
// warning logged at startup by the caller.
func New(w io.Writer, level, format string) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}

	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	return slog.New(h)
}

// Subsystem returns a logger pre-tagged with subsystem=name. Use this once
// at subsystem construction time and pass the result around.
func Subsystem(base *slog.Logger, name string) *slog.Logger {
	return base.With(slog.String("subsystem", name))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
