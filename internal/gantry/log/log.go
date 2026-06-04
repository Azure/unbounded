// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package log is the structured-logging entry point for Gantry.
//
// The design docs mandate WARN-level emission in specific places (forced
// cache eviction in §7.4, HRW rank mismatch in §5.3). This package wraps
// log/slog with a consistent attribute vocabulary so those WARN lines are
// uniformly tagged and machine-parseable.
//
// Standard attributes (use the helper constructors below to set them so
// keys don't drift):
//
//	subsystem  one of {"mirror","transfer","cache","origin","coord",
//	           "discovery","hrw","members","cdsub","agent"}
//	digest     OCI digest string ("sha256:...")
//	peer       NodeID of a remote peer
//	registry   upstream registry name
//	repo       OCI repository
//	class      §5.8 failure class
//
// Level conventions:
//
//	DEBUG  per-RPC traces, per-byte transfer milestones
//	INFO   state transitions, lifecycle events
//	WARN   §7.4 forced eviction, §5.3 HRW rank mismatch, soft failures
//	       that the design explicitly calls out
//	ERROR  hard failures requiring operator attention
package log

import (
	"context"
	"fmt"
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

// Attribute constructors. Each is a one-liner so callers don't keep typing
// the key name and so a refactor of the vocabulary only has to touch this
// file.

// Digest builds a slog.Attr carrying the standard "digest" key.
func Digest(d fmt.Stringer) slog.Attr { return slog.String("digest", safeString(d)) }

// Peer builds a slog.Attr carrying the standard "peer" key.
func Peer(p fmt.Stringer) slog.Attr { return slog.String("peer", safeString(p)) }

// Registry builds a slog.Attr carrying the standard "registry" key.
func Registry(name string) slog.Attr { return slog.String("registry", name) }

// Repo builds a slog.Attr carrying the standard "repo" key.
func Repo(name string) slog.Attr { return slog.String("repo", name) }

// Class builds a slog.Attr carrying the standard "class" key (§5.8 failure class).
func Class(c string) slog.Attr { return slog.String("class", c) }

// NodeID builds a slog.Attr carrying the standard "node_id" key.
func NodeID(id fmt.Stringer) slog.Attr { return slog.String("node_id", safeString(id)) }

// Err builds a slog.Attr carrying the standard "err" key.
func Err(err error) slog.Attr { return slog.Any("err", err) }

// Context is a tiny helper for the case where a function wants to bind
// per-request attributes onto a child logger and pass it down — e.g., a
// mirror request handler binding (registry, repo, digest) once at the top.
func Context(_ context.Context, parent *slog.Logger, attrs ...slog.Attr) *slog.Logger {
	anys := make([]any, 0, len(attrs))
	for _, a := range attrs {
		anys = append(anys, a)
	}
	return parent.With(anys...)
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

func safeString(v fmt.Stringer) string {
	if v == nil {
		return ""
	}
	return v.String()
}
