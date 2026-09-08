// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package coldstart decides how an agent resolves a digest when its local
// cache misses and the DHT lookup did not return enough providers.
//
// The mirror miss path invokes ChairResolver after a FindProviders call
// returns empty. Resolution is driven by the fixed set of Lease chairs: the
// requester ranks the chairs for the digest, asks the leading cohort to pull
// from the origin registry, and polls the DHT until one of them publishes.
// See chair.go for the cascade and the outcomes it can produce.
package coldstart

import (
	"context"
	"errors"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Sentinel errors. The mirror layer maps all of these to 5xx; tests
// distinguish them to validate which rule fired.
var (
	// ErrFailureShortCircuit reports that a chair returned a failure the
	// requester trusts enough to stop asking further chairs.
	ErrFailureShortCircuit = errors.New("coldstart: failure short-circuit")
	// ErrCooldownActive reports that the origin pull is in a transient
	// backoff window, so the requester should not pile on.
	ErrCooldownActive = errors.New("coldstart: transient cooldown active")
	// ErrExhausted fires when the cascade reaches its terminal state
	// without producing a provider (the chair ranking ran out, or
	// please_pull completed but the DHT poll timed out).
	ErrExhausted = errors.New("coldstart: cascade exhausted")
)

// Discovery is the subset of the libp2p discovery host that the
// orchestrator needs. Kept narrow for ease of mocking.
type Discovery interface {
	FindProviders(ctx context.Context, d digest.Digest) ([]ifaces.Provider, error)
	Health() float64
}

// Resolution carries the orchestrator's verdict.
type Resolution struct {
	// Providers are transfer endpoints (host:port) the caller should
	// fetch from, in priority order. Non-empty on success.
	Providers []ifaces.Provider
	// Outcome names which rule fired. Useful for tests and metrics.
	Outcome string
}

// ChildDigest is a manifest child paired with its kind. Config blobs and
// layer blobs are both pulled from /v2/<repo>/blobs/<digest> but are carried
// separately on the wire so per-kind metrics agree end-to-end across the
// please_pull boundary. internal/manifest's TypedChildren is the canonical
// producer of these values.
type ChildDigest struct {
	Digest digest.Digest
	Kind   ifaces.OriginRefKind
}

// ErrPrefetchInvalid signals a programmer error in the prefetch call:
// registry or repository was empty, which would produce a malformed
// please_pull request.
var ErrPrefetchInvalid = errors.New("coldstart: prefetch invalid arguments")

// ErrPrefetchPartial signals that at least one chair's please_pull failed.
// The other chairs were still asked to pull. Callers can errors.Is against
// this for retry or logging logic; prefetch is best-effort, so this is
// informational rather than fatal.
var ErrPrefetchPartial = errors.New("coldstart: prefetch had per-puller failures")

// isTrustedFailureClass reports whether a chair-reported failure is one the
// requester treats as authoritative. Keeping the predicate in one place
// ensures callers' definitions of "trusted" cannot drift.
func isTrustedFailureClass(class ifaces.FailureClass, trusted []ifaces.FailureClass) bool {
	for _, t := range trusted {
		if class == t {
			return true
		}
	}

	return false
}

func withoutFailureClasses(classes []ifaces.FailureClass, excluded ...ifaces.FailureClass) []ifaces.FailureClass {
	excludedSet := make(map[ifaces.FailureClass]struct{}, len(excluded))
	for _, class := range excluded {
		excludedSet[class] = struct{}{}
	}

	out := make([]ifaces.FailureClass, 0, len(classes))
	for _, class := range classes {
		if _, skip := excludedSet[class]; !skip {
			out = append(out, class)
		}
	}

	return out
}
