// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package server serves Racer HTTPS endpoints from locally validated authority.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type Server struct {
	authority *authority.Authority
	writer    client.Writer
	// Config is construction input; runtime settings are frozen on first use.
	Config             Config
	config             Config
	Lifecycle          *Lifecycle
	Leader             Leader
	once               sync.Once
	polls              *identityAdmission[wire.NodeID]
	keyringPolls       *identityAdmission[wire.NodeID]
	replicationPolls   *identityAdmission[string]
	authSlots          chan struct{}
	bootstrapSlots     chan struct{}
	writes             chan struct{}
	servingCertificate atomic.Pointer[servingCertificateReloader]
}

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
)

func (*Server) NeedLeaderElection() bool { return false }

// TLSConfig must use VerifyClientCertIfGiven: bootstrap can omit the client
// certificate, while snapshot explicitly requires VerifiedChains. Resumption
// and pooled requests must not extend certificate validity or stale trust.
// The caller owns the reload lifetime and must supply and cancel a cancelable
// context, including when listener setup fails. Start owns this context itself.
func (s *Server) TLSConfig(ctx context.Context) (*tls.Config, error) {
	s.initializeAdmission()

	if err := s.config.Validate(); err != nil {
		return nil, err
	}

	if s.authority == nil {
		return nil, wire.Unavailable
	}

	if ctx.Done() == nil {
		return nil, wire.InvalidRequest
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reloader, err := newServingCertificateReloader(s.config.TLSCertificateFile, s.config.TLSPrivateKeyFile)
	if err != nil {
		return nil, wire.Unavailable
	}

	go reloader.run(ctx, servingCertificatePollInterval)

	s.servingCertificate.Store(reloader)

	return s.tlsConfigWithCertificate(ctx, reloader.getCertificate), nil
}

func (s *Server) tlsConfigWithCertificate(ctx context.Context, certificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	base := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: certificate, ClientAuth: tls.VerifyClientCertIfGiven, SessionTicketsDisabled: true, NextProtos: []string{"http/1.1"}}
	base.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Bound trust-pool work independently of the full handshake admission
		// held by transportListener before any TLS bytes are read.
		if !take(s.authSlots) {
			return nil, wire.Overloaded
		}
		defer release(s.authSlots)

		roots, err := s.servingAuthority().TrustPool()
		if err != nil {
			// Replication uses a bearer token, not a dataplane certificate. Allow
			// TLS startup before issuer trust exists to avoid bootstrap deadlock.
			roots = x509.NewCertPool()
		}

		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.ClientCAs = roots

		return cfg, nil
	}

	s.initializeAdmission()

	return base
}

// Start opens TLS before public readiness so controller replication cannot
// deadlock on bootstrap. Process cancellation closes connections and polls.
func (s *Server) Start(ctx context.Context) error {
	s.initializeAdmission()

	if err := s.config.Validate(); err != nil {
		return err
	}

	if s.config.ControlAddress == "" || s.config.TLSCertificateFile == "" || s.config.TLSPrivateKeyFile == "" {
		return wire.InvalidRequest
	}

	if s.Lifecycle == nil || s.authority == nil {
		return wire.Unavailable
	}

	serving, cancel := context.WithCancel(ctx)
	defer cancel()

	config, err := s.TLSConfig(serving)
	if err != nil {
		return err
	}

	listener, err := (&net.ListenConfig{}).Listen(serving, "tcp", s.config.ControlAddress)
	if err != nil {
		return err
	}

	return s.serve(serving, listener, config)
}

// serve owns the listener and every accepted connection. Close, rather than a
// grace period for active traffic, is required as soon as the process stops.
func (s *Server) serve(ctx context.Context, listener net.Listener, config *tls.Config) error {
	s.initializeAdmission()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	server := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         config,
		ReadHeaderTimeout: s.config.Limits.WriteTimeout,
		ReadTimeout:       s.config.Limits.WriteTimeout,
		WriteTimeout:      wire.PollWait + 3*s.config.Limits.WriteTimeout,
		IdleTimeout:       wire.PollWait,
		MaxHeaderBytes:    s.config.Limits.HeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	server.ConnContext = connectionContext
	transport := newTransportListener(ctx, listener, config, s.config.Limits)

	done := make(chan error, 1)

	go func(done chan<- error) { done <- server.Serve(transport) }(done)

	s.Lifecycle.SetServingReady(true)

	var result error

	select {
	case err := <-done:
		result = err
		done = nil
	case <-ctx.Done():
	}

	s.Lifecycle.SetServingReady(false)

	servingCanceled := ctx.Err() != nil

	cancel()

	shutdown, stop := context.WithTimeout(context.Background(), s.config.Limits.ShutdownTimeout)
	defer stop()

	closed := make(chan error, 1)

	go func(closed chan<- error) {
		// Force-close TCP before net/http closes TLS connections: close-notify
		// can otherwise block on a slow reader. No graceful drain is allowed.
		closed <- errors.Join(transport.Close(), server.Close())
	}(closed)

	var closeErr error

	for done != nil || closed != nil {
		select {
		case result = <-done:
			done = nil
		case closeErr = <-closed:
			closed = nil
		case <-shutdown.Done():
			closeErr = errors.Join(closeErr, shutdown.Err())
			done, closed = nil, nil
		}
	}

	if errors.Is(result, http.ErrServerClosed) || servingCanceled && errors.Is(result, net.ErrClosed) {
		result = nil
	}

	return errors.Join(result, closeErr)
}

func (s *Server) initializeAdmission() {
	s.once.Do(func() {
		s.config = s.Config
		// Freeze transport settings before exposure. Authority policy was already
		// copied at construction and cannot be changed through these settings.

		s.polls = newIdentityAdmission[wire.NodeID](s.config.Limits.MaxPolls)
		s.keyringPolls = newIdentityAdmission[wire.NodeID](s.config.Limits.MaxPolls)
		s.replicationPolls = newIdentityAdmission[string](s.config.Limits.MaxConcurrentBootstrap)
		s.authSlots = make(chan struct{}, max(0, s.config.Limits.MaxConcurrentBootstrap))
		// API-backed bearer authentication must not starve local TLS authentication.
		// Enrollment and keyring bearer checks share this bounded API work pool.
		s.bootstrapSlots = make(chan struct{}, max(0, s.config.Limits.MaxConcurrentBootstrap))
		s.writes = make(chan struct{}, max(0, s.config.Limits.MaxConcurrentWrites))
	})
}

// admitPoll is the sole per-node/global poll guard. The handler must retain it
// through response Write and Flush, including error responses and aborted writes.
func (s *Server) admitPoll(node wire.NodeID) bool {
	return s.polls.acquire(node)
}

func (s *Server) releasePoll(node wire.NodeID) {
	s.polls.release(node)
}

func take(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}
func release(slots chan struct{}) { <-slots }

func (s *Server) Ready(r *http.Request) error {
	s.initializeAdmission()

	if s.Lifecycle == nil {
		return wire.Unavailable
	}

	reloader := s.servingCertificate.Load()
	if reloader == nil {
		return wire.Unavailable
	}

	certificate, err := reloader.getCertificate(nil)
	if err != nil {
		return err
	}

	// A valid chain for a different service cannot serve controller clients.
	// The reloader has already parsed and validated this immutable leaf.
	if certificate.Leaf == nil || certificate.Leaf.VerifyHostname(s.config.ReplicationServerName) != nil {
		return wire.Unavailable
	}

	if err := s.servingAuthority().TrustReady(); err != nil {
		return err
	}

	return s.Lifecycle.Ready(r)
}

// Handler uses exact paths/methods without ServeMux redirects or implicit HEAD.
func (s *Server) Handler() http.Handler {
	s.initializeAdmission()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
		// net/http permits parser slop above MaxHeaderBytes. Apply the configured
		// application bound as well, before any authentication/API work.
		headerBytes := len(r.RequestURI) + len(r.Host)
		for key, values := range r.Header {
			for _, value := range values {
				headerBytes += len(key) + len(value) + 4
			}
		}

		if s.config.Limits.HeaderBytes > 0 && headerBytes > s.config.Limits.HeaderBytes {
			writeFailure(w, wire.TooLarge)
			return
		}

		if r.URL.EscapedPath() != r.URL.Path {
			writeFailure(w, wire.InvalidRequest)
			return
		}

		var handler http.HandlerFunc

		switch {
		case r.Method == http.MethodGet && r.URL.Path == ReplicationPath && s.Leader != nil:
			if r.TLS == nil || !r.TLS.HandshakeComplete {
				writeFailure(w, wire.Unauthenticated)
				return
			}

			s.serveReplication(w, r)

			return
		case r.Method == http.MethodPost && r.URL.Path == wire.BootstrapPath:
			handler = s.serveBootstrap
		case r.Method == http.MethodGet && r.URL.Path == wire.SnapshotPath:
			handler = s.serveSnapshot
		case r.Method == http.MethodGet && r.URL.Path == wire.KeyringPath:
			handler = s.serveKeyring
		default:
			writeFailure(w, wire.InvalidRequest)
			return
		}

		if s.Ready(r) != nil {
			writeFailure(w, wire.Unavailable)
			return
		}

		if r.TLS == nil || !r.TLS.HandshakeComplete {
			writeFailure(w, wire.Unauthenticated)
			return
		}

		ctx, cancel := s.Lifecycle.ProcessContext(r.Context())
		defer cancel()

		stop := context.AfterFunc(ctx, func() {
			if conn, ok := ctx.Value(connectionKey{}).(net.Conn); ok {
				closeTransport(conn)
			}
		})
		defer stop()

		handler(w, r.WithContext(ctx))
	})
}

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

func flushResponse(ctx context.Context, w http.ResponseWriter) {
	if ctx.Err() != nil {
		panic(http.ErrAbortHandler)
	}

	responseControl(http.NewResponseController(w).Flush())

	if ctx.Err() != nil {
		panic(http.ErrAbortHandler)
	}
}

// The production HTTP/1 server supports these operations. In-memory handler
// recorders have no connection deadline; all actual connection errors abort it.
func responseControl(err error) {
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		panic(http.ErrAbortHandler)
	}
}

type connectionKey struct{}

func connectionContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, connectionKey{}, conn)
}

func closeTransport(conn net.Conn) {
	if secured, ok := conn.(*tls.Conn); ok {
		conn = secured.NetConn()
	}

	if err := conn.Close(); err != nil {
		return
	} // Already closed is harmless.
}

// TLS Close can attempt close-notify with its own deadline. Closing the raw
// transport at our deadline prevents that path from extending write admission.
func boundConnection(ctx context.Context, deadline time.Time) func() {
	conn, ok := ctx.Value(connectionKey{}).(net.Conn)
	if !ok {
		return func() {}
	}

	timer := time.AfterFunc(time.Until(deadline), func() { closeTransport(conn) })
	stop := context.AfterFunc(ctx, func() { closeTransport(conn) })

	return func() { timer.Stop(); stop() }
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

type requestWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w requestWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := w.writer.Write(b)
	if err == nil {
		err = w.ctx.Err()
	}

	return n, err
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

func writeFailure(w http.ResponseWriter, err error) {
	code := wire.Unavailable

	var protocol wire.ErrorCode
	if errors.As(err, &protocol) {
		code = protocol
	}

	var status int

	switch code {
	case wire.InvalidRequest:
		status = http.StatusBadRequest
	case wire.Unauthenticated:
		status = http.StatusUnauthorized
	case wire.Forbidden:
		status = http.StatusForbidden
	case wire.Conflict:
		status = http.StatusConflict
	case wire.TooLarge:
		status = http.StatusRequestEntityTooLarge
	case wire.UnsupportedVersion:
		status = http.StatusUpgradeRequired
	case wire.Overloaded:
		status = http.StatusTooManyRequests
	default:
		code, status = wire.Unavailable, http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if code == wire.Unavailable || code == wire.Overloaded {
		w.Header().Set("Retry-After", "1")
	}

	w.WriteHeader(status)

	encoded, encodeErr := wire.EncodeError(wire.ErrorResponse{Code: code})
	if encodeErr != nil {
		panic(http.ErrAbortHandler)
	}

	if _, err := w.Write(encoded); err != nil {
		return
	}

	responseControl(http.NewResponseController(w).Flush())
}

// Keyring polling has independent per-node and global admission, so a snapshot
// poll cannot prevent the same node from receiving the keys needed to use it.
func (s *Server) admitKeyringPoll(node wire.NodeID) bool {
	return s.keyringPolls.acquire(node)
}

func (s *Server) releaseKeyringPoll(node wire.NodeID) {
	s.keyringPolls.release(node)
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

// identityAdmission retains identities through response writes and flushes.
// Each endpoint owns an independent set and fixes its capacity at construction.
type identityAdmission[T comparable] struct {
	mu    sync.Mutex
	limit int
	held  map[T]struct{}
}

func newIdentityAdmission[T comparable](limit int) *identityAdmission[T] {
	return &identityAdmission[T]{limit: limit, held: make(map[T]struct{})}
}

func (a *identityAdmission[T]) acquire(id T) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.held[id]; exists || len(a.held) >= a.limit {
		return false
	}

	a.held[id] = struct{}{}

	return true
}

func (a *identityAdmission[T]) release(id T) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.held, id)
}

func (a *identityAdmission[T]) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.held)
}

// Config contains only the HTTPS serving inputs. Values are frozen on first use.
type Config struct {
	ControlAddress        string
	TLSCertificateFile    string
	TLSPrivateKeyFile     string
	ReplicationServerName string
	Limits                Limits
}

// Validate preserves controller validation: listener paths are checked by Start,
// and the replication client validates its server name before manager startup.
func (c Config) Validate() error {
	if c.Limits.MaxConnections <= 0 || c.Limits.MaxConcurrentHandshakes <= 0 ||
		c.Limits.MaxPolls <= 0 || c.Limits.MaxConcurrentWrites <= 0 || c.Limits.MaxConcurrentBootstrap <= 0 ||
		c.Limits.HeaderBytes <= 0 || c.Limits.WriteTimeout <= 0 || c.Limits.ShutdownTimeout <= 0 {
		return fmt.Errorf("resource names or limits: %w", wire.InvalidRequest)
	}

	return nil
}

// Leader is the publisher view needed by the internal replication endpoint.
type Leader interface {
	LeaderContext() (context.Context, bool)
	PollInterval() time.Duration
	AuthenticateReplica(context.Context, *http.Request) (string, time.Time, error)
}

const ReplicationPath = "/internal/v1/snapshot"

// New composes serving dependencies without starting work or granting readiness.
func New(cfg Config, writer client.Writer, auth *authority.Authority, lifecycle *Lifecycle, leader Leader) *Server {
	return &Server{Config: cfg, writer: writer, authority: auth, Lifecycle: lifecycle, Leader: leader}
}

func (s *Server) servingAuthority() *authority.Authority { return s.authority }

type Limits struct {
	MaxConnections          int
	MaxConcurrentHandshakes int
	MaxPolls                int
	MaxConcurrentWrites     int
	MaxConcurrentBootstrap  int
	HeaderBytes             int
	WriteTimeout            time.Duration
	ShutdownTimeout         time.Duration
}

// Lifecycle owns process serving, independently of the leader-owned publishers.
type Lifecycle struct {
	authority        *authority.Authority
	mu               sync.Mutex
	process          context.Context
	synced           bool
	serving          bool
	waitForCacheSync func(context.Context) bool
}

func NewLifecycle(a *authority.Authority) *Lifecycle {
	return &Lifecycle{authority: a}
}

func (*Lifecycle) NeedLeaderElection() bool { return false }

// ProcessContext binds a request to the serving process lifetime.
// Missing or canceled process lifetime returns an already-canceled child.
func (l *Lifecycle) ProcessContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if l == nil {
		cancel()
		return ctx, cancel
	}

	l.mu.Lock()
	process := l.process
	l.mu.Unlock()

	if process == nil {
		cancel()
		return ctx, cancel
	}

	stop := context.AfterFunc(process, cancel)
	if process.Err() != nil {
		cancel()
	}

	return ctx, func() { stop(); cancel() }
}

func (l *Lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.process != nil {
		l.mu.Unlock()
		return wire.Conflict
	}

	l.process = ctx
	l.authority.BindProcess(ctx)
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.synced, l.serving = false, false
		l.mu.Unlock()
	}()

	if l.waitForCacheSync == nil || !l.waitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}

		return wire.Unavailable
	}

	l.mu.Lock()
	l.synced = ctx.Err() == nil
	l.mu.Unlock()
	<-ctx.Done()

	return nil
}

// SetServingReady is set only after the authenticated listener is accepting.
func (l *Lifecycle) SetServingReady(ready bool) {
	l.mu.Lock()
	l.serving = ready
	l.mu.Unlock()
}

func (l *Lifecycle) Ready(_ *http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.process == nil || l.process.Err() != nil || !l.synced || !l.serving {
		return wire.Unavailable
	}

	return l.authority.PublicationReady()
}

// SetCacheSync supplies the cache barrier before Start is called.
func (l *Lifecycle) SetCacheSync(wait func(context.Context) bool) {
	l.waitForCacheSync = wait
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
