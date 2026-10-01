// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Server struct {
	// Config is construction input; runtime settings are frozen on first use.
	Config             Config
	config             Config
	Trust              *Trust
	Bootstrap          *Bootstrap
	Publications       *Publications
	Lifecycle          *Lifecycle
	Replication        *Replication
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

	if s.Bootstrap == nil || s.Bootstrap.Issuer == nil {
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

		if !take(s.authSlots) {
			return nil, wire.Overloaded
		}
		defer release(s.authSlots)

		roots, err := s.Trust.pool()
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

	if s.Lifecycle == nil || s.Publications == nil {
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

	var connections sync.Map

	server.ConnContext = connectionContext
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Store(conn, struct{}{})
			// Accept may race teardown's connection sweep. Every new connection
			// must also check cancellation after registering itself.
			if ctx.Err() != nil {
				closeTransport(conn)
			}
		}

		if state == http.StateClosed {
			connections.Delete(conn)
		}
	}

	done := make(chan error, 1)

	go func(done chan<- error) { done <- server.Serve(tls.NewListener(listener, config)) }(done)

	s.Lifecycle.SetServingReady(true)

	var result error

	select {
	case err := <-done:
		result = err
		done = nil
	case <-ctx.Done():
	}

	s.Lifecycle.SetServingReady(false)
	cancel()

	shutdown, stop := context.WithTimeout(context.Background(), s.config.Limits.ShutdownTimeout)
	defer stop()

	closed := make(chan error, 1)

	go func(closed chan<- error) {
		// Force-close TCP before net/http closes TLS connections: close-notify
		// can otherwise block on a slow reader. No graceful drain is allowed.
		connections.Range(func(key, _ any) bool {
			if conn, ok := key.(net.Conn); ok {
				closeTransport(conn)
			}

			return true
		})

		closed <- server.Close()
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

	if errors.Is(result, http.ErrServerClosed) {
		result = nil
	}

	return errors.Join(result, closeErr)
}

func (s *Server) initializeAdmission() {
	s.once.Do(func() {
		s.config = s.Config.effective()
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
	if s.Lifecycle == nil {
		return wire.Unavailable
	}

	reloader := s.servingCertificate.Load()
	if reloader == nil {
		return wire.Unavailable
	}

	if _, err := reloader.getCertificate(nil); err != nil {
		return err
	}

	if _, err := s.Trust.pool(); err != nil {
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
		case r.Method == http.MethodGet && r.URL.Path == replicationPath && s.Replication != nil:
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

	trustCtx, cancelTrust, err := s.Trust.writeContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	trustDeadline, _ := trustCtx.Deadline()

	stopTrustWrite := boundConnection(trustCtx, trustDeadline)
	defer stopTrustWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(trustDeadline))

	encoded, err := s.Bootstrap.Enroll(trustCtx, r, request)
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

func (s *Server) authenticateSnapshot(ctx context.Context, state *tls.ConnectionState) (NodeIdentity, error) {
	if !take(s.authSlots) {
		return NodeIdentity{}, wire.Overloaded
	}
	defer release(s.authSlots)

	ctx, cancel := context.WithTimeout(ctx, s.config.Limits.WriteTimeout)
	defer cancel()

	return AuthenticateCertificate(ctx, s.Trust, s.config, state)
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

	if !s.admitPoll(identity.node) {
		writeFailure(w, wire.Overloaded)

		return
	}
	defer s.releasePoll(identity.node)

	ctx, cancel := context.WithDeadline(r.Context(), identity.expires)
	defer cancel()
	// A long poll does not consume a write slot. Give its eventual response a
	// fresh bounded write window, capped by the verified chain's expiration.
	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.expires, time.Now().Add(wire.PollWait+s.config.Limits.WriteTimeout))))

	publication, err := s.Publications.Wait(ctx, identity, after)
	if !time.Now().Before(identity.expires) {
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
	trustCtx, cancelTrust, err := s.Trust.writeContext(ctx)
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
		image, err = s.Publications.Current()
		if err != nil {
			writeFailure(w, err)
			return
		}
	}

	boundedCtx, stopWindow := context.WithTimeout(trustCtx, s.config.Limits.WriteTimeout)
	defer stopWindow()

	// Keep the trust guard visible through the timeout child: context children
	// otherwise observe authority revocation only after its cancellation callback.
	writeCtx, cancelWrite, err := image.writeContext(authorityWriteContext{Context: boundedCtx, authority: trustCtx, parent: trustCtx})
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

	if _, err := publication.ForBase(r.Header.Get(wire.DeltaHeader)).writeTo(writeCtx, w); err != nil {
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

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Limits.WriteTimeout)
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

	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.expires, time.Now().Add(wire.PollWait+2*s.config.Limits.WriteTimeout))))

	bundle, err := s.Trust.waitKeyring(ctx, after)
	if !time.Now().Before(identity.expires) {
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

	trustCtx, cancelTrust, err := s.Trust.writeContext(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	ctx = trustCtx

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
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
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

	deadline := minTime(identity.expires, time.Now().Add(s.config.Limits.WriteTimeout))
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
	if _, err := io.Copy(requestWriter{ctx: ctx, writer: w}, io.LimitReader(strings.NewReader(bundle.encoded), int64(len(bundle.encoded)))); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(ctx, w)
}
