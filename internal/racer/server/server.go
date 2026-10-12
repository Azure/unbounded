// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package server serves Racer HTTPS endpoints from locally validated authority.
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type Server struct {
	authority          *authority.Authority
	writer             client.Writer
	config             Config
	Lifecycle          *Lifecycle
	Leader             Leader
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

// Config contains only the HTTPS serving inputs, copied by New.
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
		c.Limits.HeaderBytes <= 0 || c.Limits.HandshakeTimeout <= 0 || c.Limits.WriteTimeout <= 0 || c.Limits.ShutdownTimeout <= 0 {
		return fmt.Errorf("resource names or limits: %w", wire.InvalidRequest)
	}

	return nil
}

type Limits struct {
	MaxConnections          int
	MaxConcurrentHandshakes int
	MaxPolls                int
	MaxConcurrentWrites     int
	MaxConcurrentBootstrap  int
	HeaderBytes             int
	HandshakeTimeout        time.Duration
	WriteTimeout            time.Duration
	ShutdownTimeout         time.Duration
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
	return &Server{
		config: cfg, writer: writer, authority: auth, Lifecycle: lifecycle, Leader: leader,
		polls:            newIdentityAdmission[wire.NodeID](cfg.Limits.MaxPolls),
		keyringPolls:     newIdentityAdmission[wire.NodeID](cfg.Limits.MaxPolls),
		replicationPolls: newIdentityAdmission[string](cfg.Limits.MaxConcurrentBootstrap),
		authSlots:        make(chan struct{}, max(0, cfg.Limits.MaxConcurrentBootstrap)),
		// API-backed bearer authentication must not starve local TLS authentication.
		// Enrollment and keyring bearer checks share this bounded API work pool.
		bootstrapSlots: make(chan struct{}, max(0, cfg.Limits.MaxConcurrentBootstrap)),
		writes:         make(chan struct{}, max(0, cfg.Limits.MaxConcurrentWrites)),
	}
}

func (*Server) NeedLeaderElection() bool { return false }

// TLSConfig must use VerifyClientCertIfGiven: bootstrap can omit the client
// certificate, while snapshot explicitly requires VerifiedChains. Resumption
// and pooled requests must not extend certificate validity or stale trust.
// The caller owns the reload lifetime and must supply and cancel a cancelable
// context, including when listener setup fails. Start owns this context itself.
func (s *Server) TLSConfig(ctx context.Context) (*tls.Config, error) {
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

		roots, err := s.authority.TrustPool()
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

	return base
}

// Start opens TLS before public readiness so controller replication cannot
// deadlock on bootstrap. Process cancellation closes connections and polls.
func (s *Server) Start(ctx context.Context) error {
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

	certificate, err := reloader.getCertificate(nil)
	if err != nil {
		return err
	}

	// A valid chain for a different service cannot serve controller clients.
	// The reloader has already parsed and validated this immutable leaf.
	if certificate.Leaf == nil || certificate.Leaf.VerifyHostname(s.config.ReplicationServerName) != nil {
		return wire.Unavailable
	}

	if err := s.authority.TrustReady(); err != nil {
		return err
	}

	return s.Lifecycle.Ready(r)
}

// Handler uses exact paths/methods without ServeMux redirects or implicit HEAD.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))

		if s.config.Limits.HeaderBytes > 0 && requestHeaderBytes(r) > s.config.Limits.HeaderBytes {
			writeFailure(w, wire.TooLarge)
			return
		}

		if r.URL.EscapedPath() != r.URL.Path {
			writeFailure(w, wire.InvalidRequest)
			return
		}

		handler, replication := s.route(r)
		if handler == nil {
			writeFailure(w, wire.InvalidRequest)
			return
		}

		if replication {
			if r.TLS == nil || !r.TLS.HandshakeComplete {
				writeFailure(w, wire.Unauthenticated)
				return
			}

			handler(w, r)

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

func (s *Server) route(r *http.Request) (http.HandlerFunc, bool) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == ReplicationPath && s.Leader != nil:
		return s.serveReplication, true
	case r.Method == http.MethodPost && r.URL.Path == wire.BootstrapPath:
		return s.serveBootstrap, false
	case r.Method == http.MethodGet && r.URL.Path == wire.SnapshotPath:
		return s.serveSnapshot, false
	case r.Method == http.MethodGet && r.URL.Path == wire.KeyringPath:
		return s.serveKeyring, false
	default:
		return nil, false
	}
}

// net/http permits parser slop above MaxHeaderBytes. Apply the application
// bound before authentication or API work.
func requestHeaderBytes(r *http.Request) int {
	size := len(r.RequestURI) + len(r.Host)
	for key, values := range r.Header {
		for _, value := range values {
			size += len(key) + len(value) + 4
		}
	}

	return size
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

func flushAdmitted(ctx context.Context, guard *authority.Admission, w http.ResponseWriter) {
	if guard.Check(ctx) != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(ctx, w)

	if guard.Check(ctx) != nil {
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

	request, err := bootstrapRequest(r)
	if err != nil {
		writeFailure(w, err)
		return
	}

	trust, cancelTrust, err := s.authority.AdmitTrust(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	trustCtx := trust.Context()

	trustDeadline, _ := trustCtx.Deadline()

	stopTrustWrite := boundConnection(trustCtx, trustDeadline)
	defer stopTrustWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(trustDeadline))

	encoded, err := s.enroll(trustCtx, r, request)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if trust.Check(trustCtx) != nil || s.Ready(r) != nil {
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

	if trust.Check(trustCtx) != nil {
		panic(http.ErrAbortHandler)
	}

	if _, err := w.Write(encoded); err != nil || trust.Check(trustCtx) != nil {
		panic(http.ErrAbortHandler)
	}

	flushAdmitted(trustCtx, trust, w)
}

func bootstrapRequest(r *http.Request) (wire.BootstrapRequest, error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Encoding") != "" {
		return wire.BootstrapRequest{}, wire.InvalidRequest
	}

	if r.ContentLength > wire.MaxBootstrapBytes {
		return wire.BootstrapRequest{}, wire.TooLarge
	}

	return wire.DecodeBootstrap(r.Body)
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

	return s.authority.AuthenticateCertificate(ctx, state)
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

	if !s.polls.acquire(identity.Node()) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer s.polls.release(identity.Node())

	ctx, cancel := context.WithDeadline(r.Context(), identity.Expires())
	defer cancel()
	// A long poll does not consume a write slot. Give its eventual response a
	// fresh bounded write window, capped by the verified chain's expiration.
	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.Expires(), time.Now().Add(wire.PollWait+s.config.Limits.WriteTimeout))))

	publication, err := s.authority.Wait(ctx, identity, after)
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

	s.writeSnapshot(ctx, w, r, publication)
}

func (s *Server) writeSnapshot(ctx context.Context, w http.ResponseWriter, r *http.Request, publication *authority.PublicationHandle) {
	// Revalidate local trust after waiting: rotation or observed invalidity must
	// also take effect on pooled connections before returning snapshot bytes.
	trust, cancelTrust, err := s.authority.AdmitTrust(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	trustCtx := trust.Context()

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
		image, err = s.authority.Current()
		if err != nil {
			writeFailure(w, err)
			return
		}
	}

	boundedCtx, stopWindow := context.WithTimeout(trustCtx, s.config.Limits.WriteTimeout)
	defer stopWindow()

	guard, cancelWrite, err := image.AdmitWithTrust(boundedCtx, trust)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelWrite()

	writeCtx := guard.Context()

	deadline, _ := writeCtx.Deadline()

	stopWrite := boundConnection(writeCtx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if publication == nil {
		w.WriteHeader(http.StatusNoContent)
		flushAdmitted(writeCtx, guard, w)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	var sequence wire.Sequence
	if after, err := snapshotCursor(r); err == nil && after != nil {
		sequence = *after
	}

	if _, err := publication.ForBase(sequence, r.Header.Get(wire.DeltaHeader)).WriteTo(writeCtx, guard, w); err != nil {
		// A partial JSON response cannot be repaired with a protocol error.
		panic(http.ErrAbortHandler)
	}

	flushAdmitted(writeCtx, guard, w)
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

	return s.authority.Authenticate(ctx, r)
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

	if !s.keyringPolls.acquire(identity.Node()) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer s.keyringPolls.release(identity.Node())

	ctx, cancel := context.WithDeadline(r.Context(), identity.Expires())
	defer cancel()

	responseControl(http.NewResponseController(w).SetWriteDeadline(minTime(identity.Expires(), time.Now().Add(wire.PollWait+2*s.config.Limits.WriteTimeout))))

	bundle, err := s.authority.WaitKeyring(ctx, after)
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

	trust, cancelTrust, err := s.authority.AdmitTrust(ctx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer cancelTrust()

	s.writeKeyring(trust.Context(), trust, w, r, identity, after, bundle != nil)
}

func (s *Server) reauthenticateKeyring(r *http.Request, identity authority.NodeIdentity) error {
	verified, err := s.authenticateKeyring(r)

	if !time.Now().Before(identity.Expires()) {
		return wire.Unauthenticated
	}

	if err == nil && (verified.Node() != identity.Node() || verified.Cluster() != identity.Cluster()) {
		return wire.Forbidden
	}

	return err
}

func (s *Server) writeKeyring(ctx context.Context, guard *authority.Admission, w http.ResponseWriter, r *http.Request, identity authority.NodeIdentity, after *wire.Generation, changed bool) {
	accepted, err := s.authority.Keyring()
	if err != nil {
		writeFailure(w, err)
		return
	}
	// Recheck local certificate trust or live bearer authorization after waiting.
	// Never fall back from a rejected certificate to a bearer token.
	if err := s.reauthenticateKeyring(r.WithContext(ctx), identity); err != nil {
		responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.config.Limits.WriteTimeout)))
		writeFailure(w, err)

		return
	}
	// Authentication may have waited on the API. Do not deliver an old accepted
	// encoding if reconciliation invalidated or replaced it in the meantime.
	current, err := s.authority.Keyring()
	if err != nil || current != accepted || guard.Check(ctx) != nil || s.Ready(r) != nil {
		writeFailure(w, wire.Unavailable)
		return
	}

	changed = changed || after != nil && current.Generation() > *after

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	deadline := minTime(identity.Expires(), time.Now().Add(s.config.Limits.WriteTimeout))
	if freshness, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, freshness)
	}

	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	stopWrite := boundConnection(ctx, deadline)
	defer stopWrite()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Cache-Control", "no-store")

	if !changed {
		w.WriteHeader(http.StatusNoContent)
		flushAdmitted(ctx, guard, w)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Copy in bounded chunks so cancellation is observed between writes without
	// allocating a bundle-sized byte slice for each request.
	if _, err := current.Response().WriteTo(ctx, guard, w); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushAdmitted(ctx, guard, w)
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

	uid, expires, err := s.authenticateReplica(request)
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
	publication, err := s.waitReplica(ctx, after)

	if _, ok := r.LeaderContext(); !ok || !time.Now().Before(expires) {
		writeFailure(w, wire.Unavailable)
		return
	}

	unchanged := errors.Is(err, context.DeadlineExceeded)
	if unchanged {
		publication, err = s.authority.Current()
	}

	if err != nil {
		writeFailure(w, err)
		return
	}

	s.writeReplica(w, request, leader, expires, publication, unchanged)
}

func (s *Server) authenticateReplica(r *http.Request) (string, time.Time, error) {
	if !take(s.bootstrapSlots) {
		return "", time.Time{}, wire.Overloaded
	}
	defer release(s.bootstrapSlots)

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Limits.WriteTimeout)
	defer cancel()

	return s.Leader.AuthenticateReplica(ctx, r)
}

func (s *Server) waitReplica(ctx context.Context, after *wire.Sequence) (*authority.PublicationHandle, error) {
	for {
		publication, changed, err := s.authority.CurrentAndSubscribe()
		if err != nil || after == nil || publication.Sequence() > *after {
			return publication, err
		}

		select {
		case <-ctx.Done():
			return publication, ctx.Err()
		case <-changed:
		}
	}
}

func (s *Server) writeReplica(w http.ResponseWriter, request *http.Request, leader context.Context, expires time.Time, publication *authority.PublicationHandle, unchanged bool) {
	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	windowCtx, stopWrite := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(s.config.Limits.WriteTimeout)))
	defer stopWrite()

	stopLeader := context.AfterFunc(leader, stopWrite)
	defer stopLeader()

	guard, stopAuthority, err := publication.Admit(windowCtx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer stopAuthority()

	writeCtx := guard.Context()

	deadline, _ := writeCtx.Deadline()

	stopConnection := boundConnection(writeCtx, deadline)
	defer stopConnection()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if unchanged {
		w.WriteHeader(http.StatusNoContent)
		flushAdmitted(writeCtx, guard, w)

		return
	}

	if _, err := publication.ForBase(0, "").WriteTo(writeCtx, guard, w); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushAdmitted(writeCtx, guard, w)
}

// transportListener owns sockets from acceptance until close, including sockets
// not yet visible to net/http. Excess sockets are closed in the accept loop,
// without spawning a goroutine or queuing work. The kernel backlog is separate.
type transportListener struct {
	net.Listener
	config      *tls.Config
	timeout     time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	ready       chan net.Conn
	acceptDone  chan struct{}
	acceptErr   error
	connections chan struct{}
	handshakes  chan struct{}
	metrics     *transportMetrics
	mu          sync.Mutex
	closed      bool
	live        map[*transportConn]struct{}
	closeOnce   sync.Once
	closeErr    error
}

func newTransportListener(ctx context.Context, listener net.Listener, config *tls.Config, limits Limits) *transportListener {
	return newTransportListenerWithMetrics(ctx, listener, config, limits, servingTransportMetrics)
}

func newTransportListenerWithMetrics(ctx context.Context, listener net.Listener, config *tls.Config, limits Limits, metrics *transportMetrics) *transportListener {
	ctx, cancel := context.WithCancel(ctx)

	l := &transportListener{
		Listener: listener, config: config, timeout: limits.HandshakeTimeout,
		ctx: ctx, cancel: cancel, ready: make(chan net.Conn), acceptDone: make(chan struct{}),
		connections: make(chan struct{}, max(0, limits.MaxConnections)),
		handshakes:  make(chan struct{}, max(0, limits.MaxConcurrentHandshakes)),
		live:        make(map[*transportConn]struct{}),
		metrics:     metrics,
	}
	go l.run()

	return l
}

func (l *transportListener) run() {
	defer close(l.acceptDone)

	var retryDelay time.Duration

	for {
		if l.ctx.Err() != nil {
			l.acceptErr = net.ErrClosed
			return
		}

		conn, err := l.Listener.Accept()
		if err != nil {
			// Retry here, not in net/http: once this pump exits, Accept can
			// only replay acceptErr and cannot resume accepting sockets.
			var temporary net.Error
			if errors.As(err, &temporary) && temporary.Temporary() { //nolint:staticcheck // Match net/http's accept-error retry contract, including EMFILE.
				retryDelay = nextAcceptDelay(retryDelay)
				if !waitAcceptRetry(l.ctx, retryDelay) {
					l.acceptErr = net.ErrClosed
					return
				}

				continue
			}

			l.acceptErr = err

			return
		}

		retryDelay = 0

		if !take(l.connections) {
			if l.ctx.Err() == nil {
				l.metrics.connectionRejected.Inc()
			}

			closeTransport(conn)

			continue
		}

		l.metrics.connections.Inc()

		c := &transportConn{Conn: conn, owner: l}
		l.mu.Lock()

		closed := l.closed || l.ctx.Err() != nil
		if !closed {
			l.live[c] = struct{}{}
		}
		l.mu.Unlock()

		if closed {
			closeTransport(c)
			continue
		}

		if !take(l.handshakes) {
			if l.ctx.Err() == nil {
				l.metrics.handshakeRejected.Inc()
			}

			closeTransport(c)

			continue
		}

		l.metrics.handshakes.Inc()

		go l.handshake(c)
	}
}

func nextAcceptDelay(previous time.Duration) time.Duration {
	if previous == 0 {
		return 5 * time.Millisecond
	}

	return min(2*previous, time.Second)
}

func waitAcceptRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *transportListener) handshake(raw *transportConn) {
	// Only this goroutine releases handshake admission, after HandshakeContext
	// actually returns, never from a TLS configuration/verification callback.
	conn := tls.Server(raw, l.config)
	ctx, cancel := context.WithTimeout(l.ctx, l.timeout)

	err := raw.SetDeadline(time.Now().Add(l.timeout))
	if err == nil {
		err = conn.HandshakeContext(ctx)
	}

	cancel()

	var timeout net.Error
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()) {
		l.metrics.handshakeTimeouts.Inc()
	}

	l.metrics.handshakes.Dec()
	release(l.handshakes)

	if err != nil {
		closeTransport(raw)
		return
	}

	if err := raw.SetDeadline(time.Time{}); err != nil {
		closeTransport(raw)
		return
	}
	// Return the concrete *tls.Conn, not a wrapper: net/http requires that type
	// to populate Request.TLS and enforce client-certificate authentication.
	select {
	case l.ready <- conn:
	case <-l.ctx.Done():
		closeTransport(raw)
	}
}

func (l *transportListener) Accept() (net.Conn, error) {
	select {
	case <-l.acceptDone:
		return nil, l.acceptErr
	case conn := <-l.ready:
		return conn, nil
	}
}

func (l *transportListener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		l.mu.Lock()
		l.closed = true

		connections := make([]*transportConn, 0, len(l.live))
		for conn := range l.live {
			connections = append(connections, conn)
		}
		l.mu.Unlock()
		// Raw close bypasses TLS close-notify, including for handshakes and
		// completed handshakes still waiting to be handed to net/http.
		for _, conn := range connections {
			closeTransport(conn)
		}

		l.closeErr = l.Listener.Close()
	})

	return l.closeErr
}

type transportConn struct {
	net.Conn
	owner *transportListener
	once  sync.Once
	err   error
}

func (c *transportConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.live, c)
		c.owner.mu.Unlock()
		c.owner.metrics.connections.Dec()
		release(c.owner.connections)
	})

	return c.err
}

const servingCertificatePollInterval = time.Second

type servingCertificate struct {
	prefixes []servingCertificatePrefix
}

type servingCertificatePrefix struct {
	certificate         tls.Certificate
	notBefore, notAfter time.Time
}

// Published certificates are immutable. Handshakes only load a pointer and check
// its validity window; all filesystem access and parsing happens in the poller.
type servingCertificateReloader struct {
	certificateFile, keyFile string
	current                  atomic.Pointer[servingCertificate]
}

func newServingCertificateReloader(certificateFile, keyFile string) (*servingCertificateReloader, error) {
	r := &servingCertificateReloader{certificateFile: certificateFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *servingCertificateReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.getCertificateAt(time.Now())
}

func (r *servingCertificateReloader) getCertificateAt(now time.Time) (*tls.Certificate, error) {
	certificate := r.current.Load()
	if certificate == nil {
		return nil, wire.Unavailable
	}

	return certificate.at(now)
}

func (c *servingCertificate) at(now time.Time) (*tls.Certificate, error) {
	// Longest first: retain compatibility until its suffix expires, then use
	// an already validated prefix. No file reads, parsing, or verification here.
	for i := range c.prefixes {
		if prefix := &c.prefixes[i]; !now.Before(prefix.notBefore) && now.Before(prefix.notAfter) {
			return &prefix.certificate, nil
		}
	}

	return nil, wire.Unavailable
}

func (r *servingCertificateReloader) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var nextLog time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}

			if err := r.reload(); err != nil && !time.Now().Before(nextLog) {
				// Do not log paths, PEM contents, or parser errors. Bound repeated
				// failures even if a broken projection remains mounted indefinitely.
				ctrl.LoggerFrom(ctx).Error(wire.Unavailable, "serving TLS certificate reload rejected; retaining last valid certificate")

				nextLog = time.Now().Add(time.Minute)
			}
		}
	}
}

func (r *servingCertificateReloader) reload() error {
	return r.reloadAt(time.Now())
}

func (r *servingCertificateReloader) reloadAt(now time.Time) error {
	certificatePEM, keyPEM, err := r.readPair()
	if err != nil {
		return err
	}

	if err := validateCertificatePEM(certificatePEM); err != nil {
		return err
	}

	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return wire.Unavailable
	}

	validated, err := validateServingCertificate(certificate, now)
	if err != nil {
		return err
	}

	r.current.Store(validated)

	return nil
}

func (r *servingCertificateReloader) readPair() ([]byte, []byte, error) {
	certificatePath, certificateGeneration, err := servingFilePath(r.certificateFile)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	keyPath, keyGeneration, err := servingFilePath(r.keyFile)
	if err != nil || certificateGeneration != keyGeneration {
		return nil, nil, wire.Unavailable
	}

	certificatePEM, err := readServingFile(certificatePath)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	keyPEM, err := readServingFile(keyPath)
	if err != nil {
		return nil, nil, wire.Unavailable
	}
	// Recheck both names and contents. Projected paths must still name the same
	// immutable generation; standalone files must be stable across both reads.
	for _, file := range []struct {
		name, path, generation string
		contents               []byte
	}{
		{r.certificateFile, certificatePath, certificateGeneration, certificatePEM},
		{r.keyFile, keyPath, keyGeneration, keyPEM},
	} {
		path, generation, err := servingFilePath(file.name)
		if err != nil || path != file.path || generation != file.generation {
			return nil, nil, wire.Unavailable
		}

		contents, err := readServingFile(path)
		if err != nil || !bytes.Equal(contents, file.contents) {
			return nil, nil, wire.Unavailable
		}
	}

	return certificatePEM, keyPEM, nil
}

func validateCertificatePEM(certificatePEM []byte) error {
	// X509KeyPair tolerates trailing malformed PEM. Do not accidentally accept
	// a truncated chain as a valid leaf-only deployment.
	for remaining := bytes.TrimSpace(certificatePEM); len(remaining) > 0; {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return wire.Unavailable
		}

		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return wire.Unavailable
		}

		remaining = bytes.TrimSpace(rest)
	}

	return nil
}

// Kubernetes AtomicWriter projections have a ..data symlink at the volume root.
// Resolve each file and require both to belong to that same generation, including
// nested projected paths. Never follow a second generation while reading a pair.
func servingFilePath(name string) (string, string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", "", err
	}

	path, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}

	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		data := filepath.Join(dir, "..data")
		if _, err := os.Lstat(data); err == nil {
			generation, err := filepath.EvalSymlinks(data)
			if err != nil {
				return "", "", err
			}

			relative, err := filepath.Rel(generation, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", "", wire.Unavailable
			}

			return path, generation, nil
		} else if !os.IsNotExist(err) {
			return "", "", err
		}

		if filepath.Dir(dir) == dir {
			break
		}
	}

	return path, "", nil
}

func readServingFile(path string) (data []byte, result error) {
	// Reject special files before opening, and bound memory for malformed input.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, wire.Unavailable
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() { result = errors.Join(result, f.Close()) }()

	const maxBytes = 1024 * 1024

	contents, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(contents) > maxBytes {
		return nil, wire.Unavailable
	}

	return contents, nil
}

func validateServingCertificate(certificate tls.Certificate, now time.Time) (*servingCertificate, error) {
	chain, err := parseServingChain(certificate.Certificate, now)
	if err != nil {
		return nil, err
	}

	return validateServingChain(certificate, chain, now)
}

func parseServingChain(certificates [][]byte, now time.Time) ([]*x509.Certificate, error) {
	if len(certificates) == 0 {
		return nil, wire.Unavailable
	}

	chain := make([]*x509.Certificate, len(certificates))
	for i, der := range certificates {
		cert, err := x509.ParseCertificate(der)
		if err != nil || cert.NotBefore.After(now) || !cert.NotBefore.Before(cert.NotAfter) {
			return nil, wire.Unavailable
		}

		chain[i] = cert
		if i > 0 && chain[i-1].CheckSignatureFrom(cert) != nil {
			return nil, wire.Unavailable
		}
	}

	if !now.Before(chain[0].NotAfter) || chain[0].IsCA {
		return nil, wire.Unavailable
	}

	return chain, nil
}

func validateServingChain(certificate tls.Certificate, chain []*x509.Certificate, now time.Time) (*servingCertificate, error) {
	// Check the ENTIRE supplied chain's constraints before allowing any trim.
	// Expired compatibility certificates need not overlap a newly issued leaf,
	// so verify structure using copies with neutral validity windows. The actual
	// windows above and in each cached prefix are enforced separately. Raw DER,
	// signatures, EKU, name constraints, and path length constraints are unchanged.
	structural := make([]*x509.Certificate, len(chain))
	for i, cert := range chain {
		copy := *cert
		copy.NotBefore, copy.NotAfter = now.Add(-time.Hour), now.Add(time.Hour)
		structural[i] = &copy
	}

	if err := verifyServingPrefix(structural, now); err != nil {
		return nil, wire.Unavailable
	}

	certificate.Leaf = chain[0]
	validated := &servingCertificate{}

	for n := len(chain); n > 0; n-- {
		// Only a cross-signed CA starts an optional compatibility suffix. Do
		// not reinterpret an expired ordinary self-signed issuer as optional.
		if n < len(chain) && (!chain[n].IsCA || chain[n].CheckSignatureFrom(chain[n]) == nil) {
			continue
		}

		if err := verifyServingPrefix(structural[:n], now); err != nil {
			return nil, wire.Unavailable
		}

		validated.prefixes = append(validated.prefixes, servingPrefix(certificate, chain, n))
	}

	if _, err := validated.at(now); err != nil {
		return nil, err
	}

	return validated, nil
}

func servingPrefix(certificate tls.Certificate, chain []*x509.Certificate, n int) servingCertificatePrefix {
	prefix := servingCertificatePrefix{certificate: certificate, notBefore: chain[0].NotBefore, notAfter: chain[0].NotAfter}

	prefix.certificate.Certificate = certificate.Certificate[:n:n]
	for _, cert := range chain[:n] {
		if cert.NotBefore.After(prefix.notBefore) {
			prefix.notBefore = cert.NotBefore
		}

		prefix.notAfter = minTime(prefix.notAfter, cert.NotAfter)
	}

	if n < len(chain) {
		// A shorter prefix is eligible only after its compatibility suffix
		// expires, never to work around a not-yet-valid supplied certificate.
		expires := chain[n].NotAfter
		for _, cert := range chain[n:] {
			expires = minTime(expires, cert.NotAfter)
			if cert.NotBefore.After(prefix.notBefore) {
				prefix.notBefore = cert.NotBefore
			}
		}

		if expires.After(prefix.notBefore) {
			prefix.notBefore = expires
		}
	}

	return prefix
}

// Serving trust is deployment-provided, distinct from peer trust. The last
// certificate in a prefix is the local validation anchor, not a client trust
// decision. Clients must still build a path to their own trusted current CA.
func verifyServingPrefix(chain []*x509.Certificate, now time.Time) error {
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(chain[len(chain)-1])

	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return wire.Unavailable
	}

	return nil
}
