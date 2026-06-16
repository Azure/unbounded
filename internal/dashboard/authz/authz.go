// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package authz provides the dashboard's authorization abstraction.
//
// The prototype ships two implementations: a no-op Allow authorizer for local
// iteration, and a Kubernetes SubjectAccessReview-backed authorizer that
// mirrors the model already used by the unbounded-net dashboard
// (cmd/unbounded-net-controller/dashboard_auth.go). The interface keeps the
// HTTP layer independent of how authorization is decided so the SAR path can
// be hardened (per-action checks, viewer-token issuance) without reshaping the
// request handlers.
package authz

import (
	"context"
	"net/http"

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

// Subject identifies the caller whose access is being evaluated. In the SAR
// path it is populated from authenticated request identity; in the prototype's
// no-auth path it is empty.
type Subject struct {
	User   string
	Groups []string
}

// Authorizer decides whether a request may view a module surface or invoke an
// action. Implementations must be safe for concurrent use.
type Authorizer interface {
	// Subject extracts the caller identity from the request. It returns false
	// if the request is not authenticated.
	Subject(r *http.Request) (Subject, bool)
	// Allowed reports whether the subject satisfies the given permission. A nil
	// permission means "no specific permission required" and should be allowed
	// for any authenticated subject.
	Allowed(ctx context.Context, sub Subject, perm *contract.Permission) bool
}

// AllowAll authorizes every request. It is intended for local development and
// the initial in-cluster prototype where RBAC enforcement is disabled.
type AllowAll struct{}

// Subject returns an empty, always-present subject.
func (AllowAll) Subject(*http.Request) (Subject, bool) {
	return Subject{}, true
}

// Allowed always returns true.
func (AllowAll) Allowed(context.Context, Subject, *contract.Permission) bool {
	return true
}
