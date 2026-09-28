// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Keyring polling has independent per-node and global admission, so a snapshot
// poll cannot prevent the same node from receiving the keys needed to use it.
func (s *Server) admitKeyringPoll(node wire.NodeID) bool {
	s.admission.Lock()
	defer s.admission.Unlock()

	if _, exists := s.keyringPolls[node]; exists || len(s.keyringPolls) >= s.Config.Limits.MaxPolls {
		return false
	}

	s.keyringPolls[node] = struct{}{}

	return true
}

func (s *Server) releaseKeyringPoll(node wire.NodeID) {
	s.admission.Lock()
	defer s.admission.Unlock()

	delete(s.keyringPolls, node)
}

func keyringCursor(r *http.Request) (*wire.Generation, error) {
	if r.Header.Get("Content-Encoding") != "" {
		return nil, wire.InvalidRequest
	}

	// Both counters use the same canonical nonzero decimal query grammar.
	after, err := snapshotCursor(r)
	if err != nil || after == nil {
		return nil, err
	}

	generation := wire.Generation(*after)

	return &generation, nil
}

func (s *Server) authenticateKeyring(r *http.Request) (NodeIdentity, error) {
	bearer := len(r.Header.Values("Authorization")) != 0

	certificate := r.TLS != nil && (len(r.TLS.PeerCertificates) != 0 || len(r.TLS.VerifiedChains) != 0)
	if bearer && certificate {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if !bearer {
		return s.authenticateSnapshot(r.Context(), r.TLS)
	}

	if s.Bootstrap == nil {
		return NodeIdentity{}, wire.Unavailable
	}

	if !take(s.bootstrapSlots) {
		return NodeIdentity{}, wire.Overloaded
	}
	defer release(s.bootstrapSlots)

	ctx, cancel := context.WithTimeout(r.Context(), s.Config.Limits.WriteTimeout)
	defer cancel()

	return s.Bootstrap.Authenticate(ctx, r)
}

func (s *Server) serveKeyring(w http.ResponseWriter, r *http.Request) {
	after, err := keyringCursor(r)
	if err != nil {
		writeFailure(w, err)
		return
	}

	identity, err := s.authenticateKeyring(r)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if !s.admitKeyringPoll(identity.node) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer s.releaseKeyringPoll(identity.node)

	ctx, cancel := context.WithDeadline(r.Context(), identity.expires)
	defer cancel()

	trustCtx, cancelTrust, err := s.Trust.writeContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	ctx = trustCtx

	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.expires, time.Now().Add(wire.PollWait+2*s.Config.Limits.WriteTimeout))))

	bundle, err := s.Trust.waitKeyring(ctx, after)
	if !time.Now().Before(identity.expires) {
		err = wire.Unauthenticated
	}

	if err != nil {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.Config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	if ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	accepted, _, err := s.Trust.keyring()
	if err != nil {
		writeFailure(w, err)
		return
	}

	// Recheck local certificate trust or live bearer authorization after waiting.
	// Never fall back from a rejected certificate to a bearer token.
	verified, err := s.authenticateKeyring(r.WithContext(ctx))
	if !time.Now().Before(identity.expires) {
		err = wire.Unauthenticated
	}

	if err == nil && (verified.node != identity.node || verified.cluster != identity.cluster) {
		err = wire.Forbidden
	}

	if err != nil {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.Config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	// Authentication may have waited on the API. Do not deliver an old accepted
	// encoding if reconciliation invalidated or replaced it in the meantime.
	current, _, err := s.Trust.keyring()
	if err != nil || current != accepted || ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	if bundle != nil || after != nil && current.generation > *after {
		bundle = current
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	deadline := minTime(identity.expires, time.Now().Add(s.Config.Limits.WriteTimeout))
	if freshness, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, freshness)
	}

	stopWrite := boundConnection(ctx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if bundle == nil {
		w.WriteHeader(http.StatusNoContent)
		responseControl(http.NewResponseController(w).Flush())

		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Copy in bounded chunks so cancellation is observed between writes without
	// allocating a bundle-sized byte slice for each request.
	if _, err := io.Copy(requestWriter{ctx: ctx, writer: w}, io.LimitReader(strings.NewReader(bundle.encoded), int64(len(bundle.encoded)))); err != nil {
		panic(http.ErrAbortHandler)
	}

	responseControl(http.NewResponseController(w).Flush())
}
