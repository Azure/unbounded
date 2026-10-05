// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

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
// Its stores, installation proofs, signing material, and gate are private.
type Authority struct {
	config       Config
	reader       client.Reader
	client       client.Writer
	gate         *catalogGate
	publications *publicationStore
	trust        *trustStore
	publisher    *publisher
	credentials  *credentials
	bootstrap    *bootstrap
	accepted     AcceptedMembers
}

// New composes only: no I/O, cryptography, or goroutines. Config is copied.
// Dependencies must provide authoritative reads independently of discovery caches.
func New(cfg Config, deps Dependencies) *Authority {
	cfg = cfg.effective()
	c, reader := deps.Writer, deps.Reader
	a := &Authority{config: cfg, reader: reader, client: c, gate: newCatalogGate(), publications: newPublications(), trust: &trustStore{maxAge: cfg.SnapshotMaxAge}}
	a.publications.maxAge = cfg.SnapshotMaxAge
	a.accepted = make(AcceptedMembers)
	a.publisher = &publisher{Writer: c, APIReader: reader, Config: cfg, Publications: a.publications, Trust: a.trust}
	a.credentials = &credentials{Writer: c, APIReader: reader, Config: cfg, Trust: a.trust, Now: deps.Now}
	a.bootstrap = &bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: &issuer{APIReader: reader, Config: cfg, Trust: a.trust, CatalogGate: a.gate, Now: deps.Now}}
	a.bootstrap.owner = a
	a.bootstrap.Issuer.owner = a

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
type PublicationHandle struct {
	image *committedPublication
	owner *Authority
}

func (p *PublicationHandle) Sequence() wire.Sequence {
	if p == nil || p.image == nil {
		return 0
	}

	return p.image.record.Sequence
}

func (p *PublicationHandle) ForBase(hash string) Response {
	if p == nil || p.image == nil || p.owner == nil {
		return Response{}
	}

	return Response{response: p.image.ForBase(hash), owner: p.owner, image: p.image}
}

func (p *PublicationHandle) WriteContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if p == nil || p.owner == nil || p.image == nil {
		return nil, nil, wire.Unavailable
	}

	guard, cancel, err := p.image.writeContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	return responseGuard{Context: guard, owner: p.owner, image: p.image}, cancel, nil
}

// WriteContextWithTrust keeps both synchronous guards visible through a caller's
// bounded write window. A plain context child would hide synchronous revocation.
func (p *PublicationHandle) WriteContextWithTrust(window, trust context.Context) (context.Context, context.CancelFunc, error) {
	guard, ok := trust.(responseGuard)
	if !ok || p == nil || guard.owner == nil || guard.owner != p.owner || !guard.trust {
		return nil, nil, wire.Forbidden
	}

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

	return &PublicationHandle{image: p, owner: a}, nil
}

func (a *Authority) CurrentAndSubscribe() (*PublicationHandle, <-chan struct{}, error) {
	p, changed, err := a.publications.CurrentAndSubscribe()
	if err != nil {
		return nil, changed, err
	}

	return &PublicationHandle{image: p, owner: a}, changed, nil
}

func (a *Authority) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*PublicationHandle, error) {
	if identity.owner != a || a == nil {
		return nil, wire.Unauthenticated
	}

	p, err := a.publications.Wait(ctx, identity, after)
	if err != nil || p == nil {
		return nil, err
	}

	return &PublicationHandle{image: p, owner: a}, nil
}

func (a *Authority) TrustContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	guard, cancel, err := a.trust.writeContextLocked(ctx)
	if err != nil {
		return nil, nil, err
	}

	write, ok := guard.(authorityWriteContext)
	if !ok {
		cancel()
		return nil, nil, wire.Unavailable
	}

	epoch := write.authority

	return responseGuard{Context: guard, owner: a, trust: true, epoch: epoch, bundle: a.trust.bundle}, cancel, nil
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
	identity, err := authenticateCertificate(ctx, a.trust, a.config, state)
	if err != nil {
		return NodeIdentity{}, err
	}

	identity.owner = a

	return identity, nil
}

func (a *Authority) Authenticate(ctx context.Context, request *http.Request) (NodeIdentity, error) {
	return a.bootstrap.Authenticate(ctx, request)
}

func (a *Authority) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) ([]byte, error) {
	if identity.owner != a || a == nil || !identity.bearer {
		return nil, wire.Unauthenticated
	}

	return a.bootstrap.Issuer.Issue(ctx, identity, request)
}

func (a *Authority) Enroll(ctx context.Context, request *http.Request, body wire.BootstrapRequest) ([]byte, error) {
	return a.bootstrap.Enroll(ctx, request, body)
}

func (a *Authority) EnrollWithHint(ctx context.Context, request *http.Request, body wire.BootstrapRequest) ([]byte, EnrollmentHint, error) {
	return a.bootstrap.enroll(ctx, request, body)
}

// KeyringHandle is a comparable opaque view, suitable for detecting replacement
// across authentication without exposing the accepted encoding or install API.
type KeyringHandle struct {
	image *acceptedKeyring
	owner *Authority
	epoch context.Context
}

func (k KeyringHandle) Generation() wire.Generation {
	if k.image == nil {
		return 0
	}

	return k.image.generation
}

func (k KeyringHandle) Response() Response {
	if k.image == nil || k.owner == nil {
		return Response{}
	}

	return Response{response: publicationResponse{encoded: k.image.encoded}, owner: k.owner, trust: true, epoch: k.epoch, bundle: k.image}
}

// Response exposes bounded writing, never an installation proof or mutable bytes.
type Response struct {
	response publicationResponse
	owner    *Authority
	image    *committedPublication
	trust    bool
	epoch    context.Context
	bundle   *acceptedKeyring
}

type responseGuard struct {
	context.Context
	owner  *Authority
	image  *committedPublication
	trust  bool
	epoch  context.Context
	bundle *acceptedKeyring
}

func (Response) String() string   { return "<redacted authority response>" }
func (Response) GoString() string { return "<redacted authority response>" }

func (r Response) WriteTo(ctx context.Context, w io.Writer) (int64, error) {
	guard, ok := ctx.(responseGuard)
	if !ok || r.owner == nil || guard.owner != r.owner || r.trust && (!guard.trust || r.epoch == nil || r.epoch != guard.epoch || r.bundle == nil || r.bundle != guard.bundle) || r.image != nil && guard.image != r.image {
		return 0, wire.Forbidden
	}

	return r.response.writeTo(ctx, w)
}

func (a *Authority) WaitKeyring(ctx context.Context, after *wire.Generation) (*KeyringHandle, error) {
	k, err := a.trust.waitKeyring(ctx, after)
	if err != nil || k == nil {
		return nil, err
	}

	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	if k != a.trust.bundle {
		return nil, wire.Unavailable
	}

	return &KeyringHandle{image: k, owner: a, epoch: a.trust.authority}, nil
}

func (a *Authority) Keyring() (KeyringHandle, error) {
	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	if a.trust.bundle == nil || a.trust.authority == nil || a.trust.authority.Err() != nil || time.Since(a.trust.confirmed) >= a.trust.maxAge {
		return KeyringHandle{}, wire.Unavailable
	}

	return KeyringHandle{image: a.trust.bundle, owner: a, epoch: a.trust.authority}, nil
}
