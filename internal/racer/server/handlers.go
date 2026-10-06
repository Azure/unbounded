// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func (s *Server) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.bootstrapSlots)

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Limits.WriteTimeout)
	defer cancel()

	deadline, _ := ctx.Deadline()

	stopWrite := boundConnection(ctx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetReadDeadline(deadline))
	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))

	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Encoding") != "" {
		writeFailure(w, wire.InvalidRequest)
		return
	}

	if r.ContentLength > wire.MaxBootstrapBytes {
		writeFailure(w, wire.TooLarge)
		return
	}

	request, err := wire.DecodeBootstrap(r.Body)
	if err != nil {
		writeFailure(w, err)
		return
	}

	trustCtx, cancelTrust, err := s.servingAuthority().TrustContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	trustDeadline, _ := trustCtx.Deadline()

	stopTrustWrite := boundConnection(trustCtx, trustDeadline)
	defer stopTrustWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(trustDeadline))

	encoded, err := s.enroll(trustCtx, r, request)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if trustCtx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := (requestWriter{ctx: trustCtx, writer: w}).Write(encoded); err != nil || trustCtx.Err() != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(trustCtx, w)
}

func (s *Server) enroll(ctx context.Context, r *http.Request, request wire.BootstrapRequest) ([]byte, error) {
	response, hint, err := s.authority.EnrollWithHint(ctx, r, request)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithDeadline(ctx, hint.Expires)
	defer cancel()

	if err := annotateEnrollment(ctx, s.writer, hint); err != nil {
		return nil, err
	}

	return response, nil
}

func annotateEnrollment(ctx context.Context, writer client.Writer, hint authority.EnrollmentHint) error {
	node := hint.Node.DeepCopy()
	value := strconv.FormatUint(uint64(hint.Shares), 10)

	nics, err := json.Marshal(hint.RDMANICs)
	if err != nil {
		return err
	}

	nicValue := string(nics)
	if len(hint.RDMANICs) == 0 {
		nicValue = ""
	}

	_, nicPresent := node.Annotations[members.EnrolledRDMANICsAnnotation]
	if node.Annotations[members.EnrolledSharesAnnotation] == value && node.Annotations[members.EnrolledRDMANICsAnnotation] == nicValue && (nicValue != "" || !nicPresent) {
		return nil
	}

	before := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	node.Annotations[members.EnrolledSharesAnnotation] = value
	if nicValue == "" {
		delete(node.Annotations, members.EnrolledRDMANICsAnnotation)
	} else {
		node.Annotations[members.EnrolledRDMANICsAnnotation] = nicValue
	}

	return writer.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (s *Server) authenticateSnapshot(ctx context.Context, state *tls.ConnectionState) (authority.NodeIdentity, error) {
	if !take(s.authSlots) {
		return authority.NodeIdentity{}, wire.Overloaded
	}
	defer release(s.authSlots)

	ctx, cancel := context.WithTimeout(ctx, s.config.Limits.WriteTimeout)
	defer cancel()

	return s.servingAuthority().AuthenticateCertificate(ctx, state)
}

func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	after, err := snapshotCursor(r)
	if err != nil {
		writeFailure(w, err)
		return
	}

	identity, err := s.authenticateSnapshot(r.Context(), r.TLS)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if !s.admitPoll(identity.Node()) {
		writeFailure(w, wire.Overloaded)

		return
	}
	defer s.releasePoll(identity.Node())

	ctx, cancel := context.WithDeadline(r.Context(), identity.Expires())
	defer cancel()
	// A long poll does not consume a write slot. Give its eventual response a
	// fresh bounded write window, capped by the verified chain's expiration.
	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.Expires(), time.Now().Add(wire.PollWait+s.config.Limits.WriteTimeout))))

	publication, err := s.servingAuthority().Wait(ctx, identity, after)
	if !time.Now().Before(identity.Expires()) {
		err = wire.Unauthenticated
	}

	if err != nil {
		// Expiration forbids snapshot bytes, but a bounded error can still tell
		// a pooled client to recover its expired identity through bootstrap.
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	if ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}
	// Revalidate local trust after waiting: rotation or observed invalidity must
	// also take effect on pooled connections before returning snapshot bytes.
	trustCtx, cancelTrust, err := s.servingAuthority().TrustContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	if _, err := s.authenticateSnapshot(trustCtx, r.TLS); err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	image := publication
	if image == nil {
		image, err = s.servingAuthority().Current()
		if err != nil {
			writeFailure(w, err)
			return
		}
	}

	boundedCtx, stopWindow := context.WithTimeout(trustCtx, s.config.Limits.WriteTimeout)
	defer stopWindow()

	// Keep the trust guard visible through the timeout child: context children
	// otherwise observe authority revocation only after its cancellation callback.
	writeCtx, cancelWrite, err := image.WriteContextWithTrust(boundedCtx, trustCtx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelWrite()

	deadline, _ := writeCtx.Deadline()

	stopWrite := boundConnection(writeCtx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if publication == nil {
		w.WriteHeader(http.StatusNoContent)
		flushResponse(writeCtx, w)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if _, err := publication.ForBase(r.Header.Get(wire.DeltaHeader)).WriteTo(writeCtx, w); err != nil {
		// A partial JSON response cannot be repaired with a protocol error.
		panic(http.ErrAbortHandler)
	}

	flushResponse(writeCtx, w)
}

func snapshotCursor(r *http.Request) (*wire.Sequence, error) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.ForceQuery {
		return nil, wire.InvalidRequest
	}

	if r.URL.RawQuery == "" {
		return nil, nil
	}

	value, ok := strings.CutPrefix(r.URL.RawQuery, "after=")
	if !ok {
		return nil, wire.InvalidRequest
	}

	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return nil, wire.InvalidRequest
	}

	if n == 0 {
		return nil, wire.Conflict
	}

	after := wire.Sequence(n)

	return &after, nil
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

func (s *Server) authenticateKeyring(r *http.Request) (authority.NodeIdentity, error) {
	bearer := len(r.Header.Values("Authorization")) != 0

	certificate := r.TLS != nil && (len(r.TLS.PeerCertificates) != 0 || len(r.TLS.VerifiedChains) != 0)
	if bearer && certificate {
		return authority.NodeIdentity{}, wire.Unauthenticated
	}

	if !bearer {
		return s.authenticateSnapshot(r.Context(), r.TLS)
	}

	if s.authority == nil {
		return authority.NodeIdentity{}, wire.Unavailable
	}

	if !take(s.bootstrapSlots) {
		return authority.NodeIdentity{}, wire.Overloaded
	}
	defer release(s.bootstrapSlots)

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Limits.WriteTimeout)
	defer cancel()

	return s.servingAuthority().Authenticate(ctx, r)
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

	if !s.admitKeyringPoll(identity.Node()) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer s.releaseKeyringPoll(identity.Node())

	ctx, cancel := context.WithDeadline(r.Context(), identity.Expires())
	defer cancel()

	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.Expires(), time.Now().Add(wire.PollWait+2*s.config.Limits.WriteTimeout))))

	bundle, err := s.servingAuthority().WaitKeyring(ctx, after)
	if !time.Now().Before(identity.Expires()) {
		err = wire.Unauthenticated
	}

	if err != nil {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	if ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	trustCtx, cancelTrust, err := s.servingAuthority().TrustContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	ctx = trustCtx

	accepted, err := s.servingAuthority().Keyring()
	if err != nil {
		writeFailure(w, err)
		return
	}

	// Recheck local certificate trust or live bearer authorization after waiting.
	// Never fall back from a rejected certificate to a bearer token.
	verified, err := s.authenticateKeyring(r.WithContext(ctx))
	if !time.Now().Before(identity.Expires()) {
		err = wire.Unauthenticated
	}

	if err == nil && (verified.Node() != identity.Node() || verified.Cluster() != identity.Cluster()) {
		err = wire.Forbidden
	}

	if err != nil {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	// Authentication may have waited on the API. Do not deliver an old accepted
	// encoding if reconciliation invalidated or replaced it in the meantime.
	current, err := s.servingAuthority().Keyring()
	if err != nil || current != accepted || ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	if bundle != nil || after != nil && current.Generation() > *after {
		bundle = &current
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	deadline := minTime(identity.Expires(), time.Now().Add(s.config.Limits.WriteTimeout))
	if freshness, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, freshness)
	}

	stopWrite := boundConnection(ctx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if bundle == nil {
		w.WriteHeader(http.StatusNoContent)
		flushResponse(ctx, w)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Copy in bounded chunks so cancellation is observed between writes without
	// allocating a bundle-sized byte slice for each request.
	if _, err := bundle.Response().WriteTo(ctx, w); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(ctx, w)
}

func (s *Server) serveReplication(w http.ResponseWriter, request *http.Request) {
	r := s.Leader
	if _, ok := r.LeaderContext(); !ok {
		writeFailure(w, wire.Unavailable)
		return
	}

	after, err := snapshotCursor(request)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}

	authCtx, cancel := context.WithTimeout(request.Context(), s.config.Limits.WriteTimeout)
	uid, expires, err := r.AuthenticateReplica(authCtx, request)

	cancel()
	release(s.bootstrapSlots)

	if err != nil {
		writeFailure(w, err)
		return
	}

	if !s.replicationPolls.acquire(uid) {
		writeFailure(w, wire.Overloaded)

		return
	}

	defer s.replicationPolls.release(uid)

	leader, _ := r.LeaderContext()

	ctx, cancel := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(r.PollInterval())))
	defer cancel()

	stop := context.AfterFunc(leader, cancel)
	defer stop()

	responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(r.PollInterval() + s.config.Limits.WriteTimeout)))

	var publication *authority.PublicationHandle

	for {
		var changed <-chan struct{}

		publication, changed, err = s.servingAuthority().CurrentAndSubscribe()
		if err != nil || after == nil || publication.Sequence() > *after {
			break
		}

		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-changed:
		}

		if err != nil {
			break
		}
	}

	if _, ok := r.LeaderContext(); !ok || !time.Now().Before(expires) {
		writeFailure(w, wire.Unavailable)
		return
	}

	unchanged := errors.Is(err, context.DeadlineExceeded)
	if unchanged {
		publication, err = s.servingAuthority().Current()
	}

	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	windowCtx, stopWrite := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(s.config.Limits.WriteTimeout)))
	defer stopWrite()

	stopLeader := context.AfterFunc(leader, stopWrite)
	defer stopLeader()

	writeCtx, stopAuthority, err := publication.WriteContext(windowCtx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer stopAuthority()

	deadline, _ := writeCtx.Deadline()

	stopConnection := boundConnection(writeCtx, deadline)
	defer stopConnection()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if unchanged {
		w.WriteHeader(http.StatusNoContent)
		flushResponse(writeCtx, w)

		return
	}

	if _, err := publication.ForBase("").WriteTo(writeCtx, w); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(writeCtx, w)
}
