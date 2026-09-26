// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// NodeIdentity is verified output, never populated from an untrusted request.
type NodeIdentity struct {
	cluster wire.ClusterID
	node    wire.NodeID
	expires time.Time
}

func (i NodeIdentity) Node() wire.NodeID       { return i.node }
func (i NodeIdentity) Cluster() wire.ClusterID { return i.cluster }
func (i NodeIdentity) Expires() time.Time      { return i.expires }

// Issuer accesses a controller-only Secret. Its private key is never projected
// into dataplane Pods or included in a response or diagnostic.
type Issuer struct {
	APIReader client.Reader
	Config    Config
	Now       func() time.Time
}

type signingMaterial struct {
	Certificate []byte `json:"certificate"`
	PrivateKey  []byte `json:"private_key"`
}

type issuerMaterial struct {
	Pending string                     `json:"pending,omitempty"`
	Keys    map[string]signingMaterial `json:"keys"`
}

func (signingMaterial) String() string   { return "<redacted issuer>" }
func (signingMaterial) GoString() string { return "<redacted issuer>" }
func (issuerMaterial) String() string    { return "<redacted issuers>" }
func (issuerMaterial) GoString() string  { return "<redacted issuers>" }

func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	return n.Add(n, big.NewInt(1)), nil
}

func generateIssuer(now time.Time, cfg Config) ([]byte, []byte, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Racer " + string(cfg.Cluster)}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(cfg.Rotation.Interval + cfg.Rotation.PrepareFor + cfg.Rotation.RetainFor + 2*wire.CertificateLifetime), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	cert, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return nil, nil, err
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(key)

	return cert, encoded, err
}

func parseSigning(m signingMaterial) (*x509.Certificate, ed25519.PrivateKey, error) {
	cert, err := x509.ParseCertificate(m.Certificate)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	private, err := x509.ParsePKCS8PrivateKey(m.PrivateKey)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	key, ok := private.(ed25519.PrivateKey)

	pub, publicOK := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !publicOK || !pub.Equal(key.Public()) || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil {
		return nil, nil, wire.Unavailable
	}

	return cert, key, nil
}

type signingState struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
	roots       *x509.CertPool
}

func loadSigning(ctx context.Context, reader client.Reader, cfg Config, now time.Time) (signingState, error) {
	if err := ctx.Err(); err != nil {
		return signingState{}, err
	}

	if err := cfg.Validate(); err != nil {
		return signingState{}, err
	}

	topology := &TopologyReconciler{APIReader: reader, Config: cfg}

	version, _, err := topology.readVersion(ctx)
	if err != nil {
		return signingState{}, err
	}

	claim := version.Annotations[credentialClaim]
	if !strings.HasPrefix(claim, cfg.IssuerSecretName+"/"+cfg.KeyringSecretName+"/") {
		return signingState{}, wire.Unavailable
	}

	_, _, b, s, material, err := readCredentials(ctx, reader, cfg, claim)
	if err != nil {
		return signingState{}, err
	}

	cert, key, err := parseSigning(material.Keys[s.ActiveIssuer])
	if err != nil {
		return signingState{}, err
	}

	if now.Before(cert.NotBefore) || now.Add(wire.CertificateLifetime).After(cert.NotAfter) {
		return signingState{}, wire.Unavailable
	}

	roots := x509.NewCertPool()

	for _, der := range b.PeerTrustRoots {
		root, err := x509.ParseCertificate(der)
		if err != nil {
			return signingState{}, wire.Unavailable
		}

		if !now.Before(root.NotBefore) && now.Before(root.NotAfter) {
			roots.AddCert(root)
		}
	}

	if err := ctx.Err(); err != nil {
		return signingState{}, err
	}

	return signingState{certificate: cert, key: key, roots: roots}, nil
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

// TrustRoots returns a newly owned pool from authoritative committed credentials.
// Serving calls this for TLS admission and again on each snapshot request;
// cached TLS VerifiedChains alone cannot authorize a retired issuer.
// This pool is unrelated to deployment-provided HTTPS server trust.
func (i *Issuer) TrustRoots(ctx context.Context) (*x509.CertPool, error) {
	state, err := loadSigning(ctx, i.APIReader, i.Config, i.now())
	if err != nil {
		return nil, err
	}

	return state.roots, nil
}

// Issue accepts only the identity returned by token authentication. CSR names,
// extensions and requested usages are discarded. Enrollment is correlation only.
func (i *Issuer) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) (wire.BootstrapResponse, error) {
	if err := ctx.Err(); err != nil {
		return wire.BootstrapResponse{}, err
	}

	now := i.now()
	if identity.cluster != i.Config.Cluster || !wire.ValidUUID(string(identity.node)) || !identity.expires.After(now) {
		return wire.BootstrapResponse{}, wire.Forbidden
	}

	if request.Cluster != identity.cluster {
		return wire.BootstrapResponse{}, wire.Forbidden
	}

	if _, err := wire.EncodeBootstrapRequest(request); err != nil {
		return wire.BootstrapResponse{}, err
	}

	csr, err := x509.ParseCertificateRequest(request.CSRDER)
	if err != nil || csr.CheckSignature() != nil {
		return wire.BootstrapResponse{}, wire.InvalidRequest
	}

	pub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return wire.BootstrapResponse{}, wire.InvalidRequest
	}

	state, err := loadSigning(ctx, i.APIReader, i.Config, now)
	if err != nil {
		return wire.BootstrapResponse{}, err
	}

	serial, err := serialNumber()
	if err != nil {
		return wire.BootstrapResponse{}, err
	}

	uri := &url.URL{Scheme: "spiffe", Host: string(identity.cluster), Path: "/node/" + string(identity.node)}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: now, NotAfter: now.Add(wire.CertificateLifetime), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}}

	if err := ctx.Err(); err != nil {
		return wire.BootstrapResponse{}, err
	}

	leaf, err := x509.CreateCertificate(rand.Reader, template, state.certificate, pub, state.key)
	if err != nil {
		return wire.BootstrapResponse{}, wire.Unavailable
	}

	response := wire.BootstrapResponse{SchemaVersion: wire.SchemaVersion, Cluster: identity.cluster, Node: identity.node, Enrollment: request.Enrollment, CertificateChain: [][]byte{leaf, state.certificate.Raw}}
	if _, err := wire.EncodeBootstrap(response); err != nil {
		return wire.BootstrapResponse{}, err
	}

	if err := ctx.Err(); err != nil {
		return wire.BootstrapResponse{}, err
	}

	return response, nil
}

// AuthenticateCertificate requires a verified chain, the client-auth usage,
// cluster-scoped Node URI SAN, current validity, and authorization. Recheck on
// every poll: an existing TLS connection must not bypass certificate expiry.
func AuthenticateCertificate(ctx context.Context, reader, hints client.Reader, cfg Config, state *tls.ConnectionState) (NodeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if state == nil || !state.HandshakeComplete || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return NodeIdentity{}, wire.Unauthenticated
	}

	leaf := state.PeerCertificates[0]

	now := time.Now()
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || len(leaf.UnknownExtKeyUsage) != 0 || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if len(leaf.URIs) != 1 {
		return NodeIdentity{}, wire.Unauthenticated
	}

	uri := leaf.URIs[0]

	node := wire.NodeID(strings.TrimPrefix(uri.Path, "/node/"))
	if !wire.ValidUUID(string(node)) || uri.String() != "spiffe://"+uri.Host+"/node/"+string(node) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if uri.Host != string(cfg.Cluster) {
		return NodeIdentity{}, wire.Forbidden
	}

	if reader == nil {
		return NodeIdentity{}, wire.Unavailable
	}

	roots, err := (&Issuer{APIReader: reader, Config: cfg}).TrustRoots(ctx)
	if err != nil {
		return NodeIdentity{}, wire.Unavailable
	}

	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return NodeIdentity{}, wire.Unauthenticated
	}

	expires := leaf.NotAfter
	for _, cert := range chains[0] {
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}

	if err := authorizeNode(ctx, reader, hints, cfg, node); err != nil {
		return NodeIdentity{}, err
	}

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if !time.Now().Before(expires) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	return NodeIdentity{cluster: cfg.Cluster, node: node, expires: expires}, nil
}
