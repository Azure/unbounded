// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"time"
)

type tlsSnapshot struct {
	bundle      TrustBundle
	certificate tls.Certificate
	roots       *x509.CertPool
	root        string
}

// HotTLS replaces immutable TLS snapshots only after complete validation.
type HotTLS struct {
	mu      sync.RWMutex
	current *tlsSnapshot
}

func NewHotTLS() *HotTLS { return &HotTLS{} }

func (h *HotTLS) snapshot() (*tlsSnapshot, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.current == nil {
		return nil, ErrNotReady
	}

	return h.current, nil
}

func (h *HotTLS) Update(bundleJSON, certPEM, keyPEM []byte) error {
	b, err := ParseBundle(bundleJSON)
	if err != nil {
		return err
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}

	if leaf.IsCA || len(leaf.URIs) != 1 {
		return errors.New("invalid TLS leaf identity")
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(b.Certificates))

	intermediates := x509.NewCertPool()

	for _, der := range pair.Certificate[1:] {
		cert, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			return parseErr
		}

		intermediates.AddCert(cert)
	}

	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		return err
	}

	pair.Leaf = leaf
	snapshot := &tlsSnapshot{bundle: b, certificate: pair, roots: roots, root: digest(chains[0][len(chains[0])-1].Raw)}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.current != nil {
		old := h.current.bundle
		if b.Generation < old.Generation || (b.Generation == old.Generation && b.Digest() != old.Digest()) {
			return errors.New("trust update rolls back or equivocates")
		}
	}

	h.current = snapshot

	return nil
}

func (s *tlsSnapshot) config(auth tls.ClientAuthType) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.certificate}, RootCAs: s.roots, ClientCAs: s.roots, ClientAuth: auth, SessionTicketsDisabled: true, NextProtos: []string{"http/1.1"}}
}

// ServerConfig hot-loads a complete immutable context for every new connection.
// Existing connections retain their original context and must be drained by the
// parent transport before it reports OldConnectionsDrained.
func (h *HotTLS) ServerConfig(auth tls.ClientAuthType) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		s, err := h.snapshot()
		if err != nil {
			return nil, err
		}

		return s.config(auth), nil
	}}
}

// HandshakeProof performs a NEW full authenticated TLS handshake on raw
// transport. For client mode serverName must match the peer's DNS identity, or
// be empty to use URI-only verification (RecordTLSProof checks the exact URI).
// Server mode requires a verified node client certificate. Client mode verifies
// the CP server certificate without requiring mutual TLS; CP leaves have only
// serverAuth. The enrolled peer fingerprint binds the exact pod and boot.
// The caller owns conn and must close it on errors. ack must come from the
// authenticated exchange on this session via the returned finish function, not
// from a different connection or unauthenticated headers. Calling finish after
// the transport has validated the process's acknowledgment seals the proof.
func (h *HotTLS) HandshakeProof(ctx context.Context, raw net.Conn, server bool, serverName string) (*tls.Conn, func(Acknowledgment) (Proof, error), error) {
	if _, ok := raw.(*tls.Conn); ok {
		return nil, nil, errors.New("proof requires a new raw transport")
	}

	s, err := h.snapshot()
	if err != nil {
		return nil, nil, err
	}

	config := s.config(tls.RequireAndVerifyClientCert)

	var conn *tls.Conn
	if server {
		conn = tls.Server(raw, config)
	} else {
		config.Certificates = nil
		config.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if err := info.SupportsCertificate(&s.certificate); err != nil {
				return nil, err
			}

			return &s.certificate, nil
		}
		config.ServerName = serverName
		// URI-only peers intentionally lack DNS SANs; perform full chain and
		// serverAuth verification here, then bind their URI in RecordTLSProof.
		if serverName == "" {
			config.InsecureSkipVerify = true // Full verification is supplied below.
			config.VerifyConnection = func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("missing peer certificate")
				}

				pool := x509.NewCertPool()
				for _, cert := range cs.PeerCertificates[1:] {
					pool.AddCert(cert)
				}

				_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: s.roots, Intermediates: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})

				return err
			}
		}

		conn = tls.Client(raw, config)
	}

	if err = conn.HandshakeContext(ctx); err != nil {
		return nil, nil, err
	}

	cs := conn.ConnectionState()
	if !cs.HandshakeComplete || cs.DidResume || len(cs.PeerCertificates) == 0 {
		return nil, nil, errors.New("proof requires a full verified handshake")
	}

	peer := cs.PeerCertificates[0]
	if len(peer.URIs) != 1 {
		return nil, nil, errors.New("peer must have exactly one URI")
	}

	usage := x509.ExtKeyUsageServerAuth
	if server {
		usage = x509.ExtKeyUsageClientAuth
	}

	pool := x509.NewCertPool()
	for _, cert := range cs.PeerCertificates[1:] {
		pool.AddCert(cert)
	}

	chains, err := peer.Verify(x509.VerifyOptions{Roots: s.roots, Intermediates: pool, KeyUsages: []x509.ExtKeyUsage{usage}, DNSName: serverName})
	if err != nil {
		return nil, nil, err
	}

	proof := Proof{peerFingerprint: digest(peer.Raw), peerURI: peer.URIs[0].String(), peerRoot: digest(chains[0][len(chains[0])-1].Raw), localRoot: s.root, bundleDigest: s.bundle.Digest(), at: time.Now(), peerIsServer: !server}

	var once sync.Once

	finish := func(ack Acknowledgment) (Proof, error) {
		var result Proof

		used := true

		once.Do(func() { used = false; result = proof; result.ack = ack })

		if used {
			return Proof{}, errors.New("TLS proof already consumed")
		}

		return result, nil
	}

	return conn, finish, nil
}
