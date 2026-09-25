// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Server struct {
	Config         Config
	APIReader      client.Reader
	Bootstrap      *Bootstrap
	Publications   *Publications
	Lifecycle      *Lifecycle
	once           sync.Once
	admission      sync.Mutex
	polls          map[wire.NodeID]struct{}
	bootstrapSlots chan struct{}
	writes         chan struct{}
}

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
)

func (*Server) NeedLeaderElection() bool { return true }

// TLSConfig must use VerifyClientCertIfGiven: bootstrap can omit the client
// certificate, while snapshot explicitly requires VerifiedChains. Resumption
// and pooled requests must not extend certificate validity or stale trust.
func (s *Server) TLSConfig(ctx context.Context) (*tls.Config, error) {
	if err := s.Config.Validate(); err != nil {
		return nil, err
	}

	if s.Bootstrap == nil || s.Bootstrap.Issuer == nil {
		return nil, wire.Unavailable
	}

	certificate, err := tls.LoadX509KeyPair(s.Config.TLSCertificateFile, s.Config.TLSPrivateKeyFile)
	if err != nil {
		return nil, wire.Unavailable
	}

	return s.tlsConfig(ctx, certificate), nil
}

func (s *Server) tlsConfig(ctx context.Context, certificate tls.Certificate) *tls.Config {
	base := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.VerifyClientCertIfGiven, SessionTicketsDisabled: true, NextProtos: []string{"http/1.1"}}
	base.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		lookup, cancel := context.WithTimeout(hello.Context(), s.Config.Limits.WriteTimeout)
		defer cancel()

		stop := context.AfterFunc(ctx, cancel)
		defer stop()

		if !take(s.bootstrapSlots) {
			return nil, wire.Overloaded
		}
		defer release(s.bootstrapSlots)

		roots, err := s.Bootstrap.Issuer.TrustRoots(lookup)
		if err != nil {
			return nil, wire.Unavailable
		}

		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.ClientCAs = roots

		return cfg, nil
	}

	s.initializeAdmission()

	return base
}

// Start waits for synchronized inputs and initialized issuer/publication state.
// Leadership cancellation closes listeners/connections and cancels every poll.
func (s *Server) Start(ctx context.Context) error {
	if err := s.Config.Validate(); err != nil {
		return err
	}

	if s.Config.ControlAddress == "" || s.Config.TLSCertificateFile == "" || s.Config.TLSPrivateKeyFile == "" {
		return wire.InvalidRequest
	}

	if s.Lifecycle == nil || s.Publications == nil {
		return wire.Unavailable
	}

	if err := s.Lifecycle.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}

		return err
	}

	serving, cancel := s.leaderContext(ctx)
	defer cancel()

	config, err := s.TLSConfig(serving)
	if err != nil {
		return err
	}

	listener, err := (&net.ListenConfig{}).Listen(serving, "tcp", s.Config.ControlAddress)
	if err != nil {
		return err
	}

	return s.serve(serving, listener, config)
}

// serve owns the listener and every accepted connection. Close, rather than a
// grace period for active traffic, is required as soon as leadership is lost.
func (s *Server) serve(ctx context.Context, listener net.Listener, config *tls.Config) (result error) {
	server := &http.Server{Handler: s.Handler(), TLSConfig: config, ReadHeaderTimeout: s.Config.Limits.WriteTimeout, ReadTimeout: s.Config.Limits.WriteTimeout, WriteTimeout: wire.PollWait + 3*s.Config.Limits.WriteTimeout, IdleTimeout: wire.PollWait, MaxHeaderBytes: s.Config.Limits.HeaderBytes, BaseContext: func(net.Listener) context.Context { return ctx }}

	var connections sync.Map

	server.ConnContext = connectionContext
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Store(conn, struct{}{})
		}

		if state == http.StateClosed {
			connections.Delete(conn)
		}

		if ctx.Err() != nil {
			closeTransport(conn)
		}
	}

	closeAll := func() {
		connections.Range(func(key, _ any) bool {
			if conn, ok := key.(net.Conn); ok {
				closeTransport(conn)
			}

			return true
		})
	}

	stop := context.AfterFunc(ctx, closeAll)
	defer stop()
	defer s.Lifecycle.SetServingReady(false)
	defer func() { result = errors.Join(result, server.Close()) }()

	done := make(chan error, 1)

	go func() { done <- server.Serve(tls.NewListener(listener, config)) }()

	s.Lifecycle.SetServingReady(true)

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case <-ctx.Done():
		s.Lifecycle.SetServingReady(false)
		closeAll()

		closeErr := server.Close()

		shutdown, cancel := context.WithTimeout(context.Background(), s.Config.Limits.ShutdownTimeout)
		defer cancel()

		shutdownErr := server.Shutdown(shutdown)
		select {
		case <-done:
			return errors.Join(closeErr, shutdownErr)
		case <-shutdown.Done():
			return errors.Join(closeErr, shutdownErr, shutdown.Err())
		}
	}
}

func (s *Server) initializeAdmission() {
	s.once.Do(func() {
		s.polls = make(map[wire.NodeID]struct{})
		s.bootstrapSlots = make(chan struct{}, max(0, s.Config.Limits.MaxConcurrentBootstrap))
		s.writes = make(chan struct{}, max(0, s.Config.Limits.MaxConcurrentWrites))
	})
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

func (s *Server) leaderContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	s.Lifecycle.mu.Lock()
	leader := s.Lifecycle.leader
	s.Lifecycle.mu.Unlock()

	if leader == nil {
		cancel()
		return ctx, cancel
	}

	stop := context.AfterFunc(leader, cancel)
	if leader.Err() != nil {
		cancel()
	}

	return ctx, func() { stop(); cancel() }
}

func (s *Server) Ready(r *http.Request) error {
	if s.Lifecycle == nil {
		return wire.Unavailable
	}

	return s.Lifecycle.Ready(r)
}

// Handler uses exact paths/methods without ServeMux redirects or implicit HEAD.
func (s *Server) Handler() http.Handler {
	s.initializeAdmission()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.Config.Limits.WriteTimeout)))
		// net/http permits parser slop above MaxHeaderBytes. Apply the configured
		// application bound as well, before any authentication/API work.
		headerBytes := len(r.RequestURI) + len(r.Host)
		for key, values := range r.Header {
			for _, value := range values {
				headerBytes += len(key) + len(value) + 4
			}
		}

		if s.Config.Limits.HeaderBytes > 0 && headerBytes > s.Config.Limits.HeaderBytes {
			writeFailure(w, wire.TooLarge)
			return
		}

		if r.URL.EscapedPath() != r.URL.Path || (r.Method != http.MethodPost || r.URL.Path != wire.BootstrapPath) && (r.Method != http.MethodGet || r.URL.Path != wire.SnapshotPath) {
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

		ctx, cancel := s.leaderContext(r.Context())
		defer cancel()

		stop := context.AfterFunc(ctx, func() {
			if conn, ok := ctx.Value(connectionKey{}).(net.Conn); ok {
				closeTransport(conn)
			}
		})
		defer stop()

		r = r.WithContext(ctx)
		if r.Method == http.MethodPost {
			s.serveBootstrap(w, r)
		} else {
			s.serveSnapshot(w, r)
		}
	})
}

func (s *Server) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.bootstrapSlots)

	ctx, cancel := context.WithTimeout(r.Context(), s.Config.Limits.WriteTimeout)
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

	response, err := s.Bootstrap.Enroll(ctx, r, request)
	if err != nil {
		writeFailure(w, err)
		return
	}

	encoded, err := wire.EncodeBootstrap(response)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if ctx.Err() != nil || s.Ready(r) != nil {
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

	if _, err := w.Write(encoded); err == nil {
		responseControl(http.NewResponseController(w).Flush())
	}
}

func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	after, err := snapshotCursor(r)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}

	auth, cancel := context.WithTimeout(r.Context(), s.Config.Limits.WriteTimeout)
	identity, err := AuthenticateCertificate(auth, s.APIReader, s.Config, r.TLS)

	cancel()
	release(s.bootstrapSlots)

	if err != nil {
		writeFailure(w, err)
		return
	}

	s.admission.Lock()

	_, exists := s.polls[identity.node]
	if exists || len(s.polls) >= s.Config.Limits.MaxPolls {
		s.admission.Unlock()
		writeFailure(w, wire.Overloaded)

		return
	}

	s.polls[identity.node] = struct{}{}
	s.admission.Unlock()

	defer func() { s.admission.Lock(); delete(s.polls, identity.node); s.admission.Unlock() }()

	ctx, cancel := context.WithDeadline(r.Context(), identity.expires)
	defer cancel()
	// A long poll does not consume a write slot. Give its eventual response a
	// fresh bounded write window, capped by the verified chain's expiration.
	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.expires, time.Now().Add(wire.PollWait+s.Config.Limits.WriteTimeout))))

	publication, err := s.Publications.Wait(ctx, identity, after)
	if !time.Now().Before(identity.expires) {
		err = wire.Unauthenticated
	}

	if err != nil {
		// Expiration forbids snapshot bytes, but a bounded error can still tell
		// a pooled client to recover its expired identity through bootstrap.
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.Config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}

	if ctx.Err() != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}
	// Revalidate after waiting too: a rotation or live revocation during the
	// poll must not disclose a newly published snapshot to a revoked identity.
	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}

	auth, stop := context.WithTimeout(ctx, s.Config.Limits.WriteTimeout)
	_, err = AuthenticateCertificate(auth, s.APIReader, s.Config, r.TLS)

	stop()
	release(s.bootstrapSlots)

	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	deadline := minTime(identity.expires, time.Now().Add(s.Config.Limits.WriteTimeout))

	stopWrite := boundConnection(ctx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if publication == nil {
		w.WriteHeader(http.StatusNoContent)
		responseControl(http.NewResponseController(w).Flush())

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if _, err := publication.WriteTo(requestWriter{ctx: ctx, writer: w}); err != nil {
		// A partial JSON response cannot be repaired with a protocol error.
		panic(http.ErrAbortHandler)
	}

	responseControl(http.NewResponseController(w).Flush())
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

	return func() { timer.Stop() }
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

	return w.writer.Write(b)
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

	statuses := map[wire.ErrorCode]int{wire.InvalidRequest: 400, wire.Unauthenticated: 401, wire.Forbidden: 403, wire.Conflict: 409, wire.TooLarge: 413, wire.UnsupportedVersion: 426, wire.Overloaded: 429, wire.Unavailable: 503}

	status, ok := statuses[code]
	if !ok {
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
