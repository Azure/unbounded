// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Authority owns durable credential and publication operations and their shared
// admission gate. No operation exposes installation proofs or issuer material.
// The legacy engines remain private implementation details during extraction.
type Authority struct {
	config       Config
	reader       client.Reader
	client       client.Client
	gate         *CatalogGate
	publications *Publications
	trust        *Trust
	publisher    *TopologyReconciler
	credentials  *KeyringReconciler
	bootstrap    *Bootstrap
	accepted     AcceptedMembers
}

// NewAuthority composes only: no I/O, cryptography, or goroutines. Config is
// copied; narrowing its fields is deferred to the package migration. Dependencies
// must provide authoritative reads independently of the discovery cache.
func NewAuthority(cfg Config, c client.Client, reader client.Reader) *Authority {
	cfg = cfg.effective()
	a := &Authority{config: cfg, reader: reader, client: c, gate: newCatalogGate(), publications: NewPublications(), trust: &Trust{maxAge: cfg.SnapshotMaxAge}}
	a.publications.maxAge = cfg.SnapshotMaxAge
	a.accepted = make(AcceptedMembers)
	a.publisher = &TopologyReconciler{Client: c, APIReader: reader, Config: cfg, Publications: a.publications, Trust: a.trust, CatalogGate: a.gate, authority: a, Accepted: make(AcceptedMembers)}
	a.credentials = &KeyringReconciler{Client: c, APIReader: reader, Config: cfg, Trust: a.trust, CatalogGate: a.gate, authority: a}
	a.bootstrap = &Bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: &Issuer{APIReader: reader, Config: cfg, Trust: a.trust, CatalogGate: a.gate}}

	return a
}

// Recover validates permanent installation state without granting serving rights.
func (a *Authority) Recover(ctx context.Context, writer client.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := a.config.Validate(); err != nil {
		return err
	}

	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	if err := ensureInstalled(ctx, writer, a.reader, a.config); err != nil {
		return fmt.Errorf("ensure Racer installation: %w", err)
	}

	if _, _, err := readVersion(ctx, a.reader, a.config); err != nil {
		return fmt.Errorf("recover Racer installation: %w", err)
	}

	return nil
}

// PublicationHandle is a response-only view. It cannot be installed or advanced.
type PublicationHandle struct{ image *CommittedPublication }

func (p *PublicationHandle) Sequence() wire.Sequence { return p.image.record.Sequence }
func (p *PublicationHandle) ForBase(hash string) Response {
	return Response{response: p.image.ForBase(hash)}
}

func (p *PublicationHandle) WriteContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if p == nil {
		return nil, nil, wire.Unavailable
	}

	return p.image.writeContext(ctx)
}

// WriteContextWithTrust keeps both synchronous guards visible through a caller's
// bounded write window. A plain context child would hide synchronous revocation.
func (p *PublicationHandle) WriteContextWithTrust(window, trust context.Context) (context.Context, context.CancelFunc, error) {
	return p.WriteContext(authorityWriteContext{Context: window, authority: trust, parent: trust})
}

func (a *Authority) TrustReady() error               { _, err := a.trust.pool(); return err }
func (a *Authority) PublicationReady() error         { _, err := a.publications.Current(); return err }
func (a *Authority) BindProcess(ctx context.Context) { a.publications.bindProcess(ctx) }

func (a *Authority) Current() (*PublicationHandle, error) {
	p, err := a.publications.Current()
	if err != nil {
		return nil, err
	}

	return &PublicationHandle{image: p}, nil
}

func (a *Authority) CurrentAndSubscribe() (*PublicationHandle, <-chan struct{}, error) {
	p, changed, err := a.publications.CurrentAndSubscribe()
	if err != nil {
		return nil, changed, err
	}

	return &PublicationHandle{image: p}, changed, nil
}

func (a *Authority) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*PublicationHandle, error) {
	p, err := a.publications.Wait(ctx, identity, after)
	if err != nil || p == nil {
		return nil, err
	}

	return &PublicationHandle{image: p}, nil
}

func (a *Authority) TrustContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	return a.trust.writeContext(ctx)
}

// TrustPool returns an independent pool: TLS callers cannot mutate local trust.
func (a *Authority) TrustPool() (*x509.CertPool, error) {
	p, err := a.trust.pool()
	if err != nil {
		return nil, err
	}

	return p.Clone(), nil
}

func (a *Authority) AuthenticateCertificate(ctx context.Context, state *tls.ConnectionState) (NodeIdentity, error) {
	return AuthenticateCertificate(ctx, a.trust, a.config, state)
}

func (a *Authority) Authenticate(ctx context.Context, request *http.Request) (NodeIdentity, error) {
	return a.bootstrap.Authenticate(ctx, request)
}

func (a *Authority) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) ([]byte, error) {
	return a.bootstrap.Issuer.Issue(ctx, identity, request)
}

func (a *Authority) Enroll(ctx context.Context, request *http.Request, body wire.BootstrapRequest) ([]byte, error) {
	return a.bootstrap.Enroll(ctx, request, body)
}

// KeyringHandle is a comparable opaque view, suitable for detecting replacement
// across authentication without exposing the accepted encoding or install API.
type KeyringHandle struct{ image *acceptedKeyring }

func (k KeyringHandle) Generation() wire.Generation { return k.image.generation }
func (k KeyringHandle) Response() Response {
	return Response{response: publicationResponse{encoded: k.image.encoded}}
}

// Response exposes bounded writing, never an installation proof or mutable bytes.
type Response struct{ response publicationResponse }

func (Response) String() string   { return "<redacted authority response>" }
func (Response) GoString() string { return "<redacted authority response>" }

func (r Response) WriteTo(ctx context.Context, w io.Writer) (int64, error) {
	return r.response.writeTo(ctx, w)
}

func (a *Authority) WaitKeyring(ctx context.Context, after *wire.Generation) (*KeyringHandle, error) {
	k, err := a.trust.waitKeyring(ctx, after)
	if err != nil || k == nil {
		return nil, err
	}

	return &KeyringHandle{image: k}, nil
}

func (a *Authority) Keyring() (KeyringHandle, error) {
	k, _, err := a.trust.keyring()
	return KeyringHandle{image: k}, err
}

// Legacy literal fixtures can still assemble individual engines. Production
// always shares the single owner constructed by Assemble.
func (s *Server) servingAuthority() *Authority {
	if s.authority != nil {
		return s.authority
	}

	return &Authority{config: s.config, trust: s.Trust, publications: s.Publications, bootstrap: s.Bootstrap}
}
