// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactsource

import (
	"context"
	"log/slog"
	"sort"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// Sources maps a redacted source name to a parsed artifact source. Source names
// are safe to include in preflight output; raw source strings may contain
// sensitive data and must not be used as map keys.
type Sources map[string]Source

// ReachabilityChecker checks that a set of artifact sources is reachable.
type ReachabilityChecker struct {
	Log        *slog.Logger
	CheckName  string
	Target     string
	OKMessage  string
	ErrMessage string
	Sources    func() (Sources, error)
}

// Name returns the preflight check name.
func (c ReachabilityChecker) Name() string { return c.CheckName }

// Check probes every configured artifact source and reports failures using only
// the redacted source names.
func (c ReachabilityChecker) Check(ctx context.Context) []preflight.Result {
	sources, err := c.Sources()
	if err != nil {
		return preflight.ResultsError(c.CheckName, c.Target, "artifact sources could not be resolved")
	}

	var failures []preflight.Result

	for _, sourceName := range sortedSourceNames(sources) {
		source := sources[sourceName]
		if err := source.Probe(ctx); err != nil {
			if c.Log != nil {
				c.Log.Debug("preflight artifact source probe failed", slog.String("check", c.CheckName), slog.String("source", sourceName))
			}

			failures = append(failures, preflight.Error(c.CheckName, c.Target, "%s: %s", c.ErrMessage, sourceName))
		}
	}

	if len(failures) > 0 {
		return failures
	}

	return preflight.ResultsOK(c.CheckName, c.Target, c.OKMessage)
}

func sortedSourceNames(sources Sources) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
