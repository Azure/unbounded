// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package chaircall provides the TLS identity used by the HTTPS transport for
// the cold-start please_pull RPC.
//
// please_pull carries the requester's delegated registry Authorization header,
// which the chair uses to pull on its behalf, so the transport must be
// encrypted. Rather than introduce a certificate authority, peers are
// authenticated by pinning the TLS certificate to the chair's libp2p peer ID,
// which the chair Lease already publishes alongside its address. The
// certificate is self-signed with the agent's libp2p identity key, so a client
// can recover the peer ID from it and compare. That reuses the identity the
// cluster already trusts and needs no issuance or rotation machinery.
package chaircall

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// alpnProtocols are the standard HTTP ALPN identifiers. A bespoke protocol
// name would be rejected by net/http's server, and it is unnecessary: the peer
// ID pin already establishes that the listener is the expected chair.
var alpnProtocols = []string{"h2", "http/1.1"}

// certValidity is deliberately long: the certificate is not a trust anchor,
// the peer ID pin is. Expiry only bounds how long a leaked key is useful.
const certValidity = 365 * 24 * time.Hour

// ServerTLSConfig builds the chair listener's TLS configuration from the
// agent's libp2p identity. The certificate is self-signed with the identity
// key so a client can recover the peer ID from it.
func ServerTLSConfig(priv crypto.PrivKey) (*tls.Config, error) {
	cert, err := selfSignedCert(priv)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   alpnProtocols,
	}, nil
}

// ClientTLSConfig builds a configuration that accepts exactly one peer.
//
// Certificate verification is delegated to VerifyPeerCertificate because the
// certificate is self-signed and there is no chain to validate; the peer ID
// derived from its public key is the whole of the trust decision.
func ClientTLSConfig(expected peer.ID) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify:    true, //nolint:gosec // VerifyPeerCertificate pins the peer ID instead
		MinVersion:            tls.VersionTLS13,
		NextProtos:            alpnProtocols,
		VerifyPeerCertificate: verifyPeerID(expected),
	}
}

func selfSignedCert(priv crypto.PrivKey) (tls.Certificate, error) {
	std, err := crypto.PrivKeyToStdKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("chaircall: convert identity key: %w", err)
	}

	ed, ok := std.(*ed25519.PrivateKey)
	if !ok {
		edVal, okVal := std.(ed25519.PrivateKey)
		if !okVal {
			return tls.Certificate{}, fmt.Errorf("chaircall: identity key is %T, want ed25519", std)
		}

		ed = &edVal
	}

	self, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("chaircall: derive peer id: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("chaircall: serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: self.String()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, ed.Public(), *ed)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("chaircall: create certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: *ed}, nil
}

func verifyPeerID(expected peer.ID) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("chaircall: peer presented no certificate")
		}

		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("chaircall: parse peer certificate: %w", err)
		}

		got, err := PeerIDFromCertificate(cert)
		if err != nil {
			return err
		}

		if got != expected {
			return fmt.Errorf("chaircall: peer is %s, want %s", got, expected)
		}

		return nil
	}
}

// PeerIDFromCertificate recovers the libp2p peer ID a certificate was signed
// with.
func PeerIDFromCertificate(cert *x509.Certificate) (peer.ID, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("chaircall: certificate key is %T, want ed25519", cert.PublicKey)
	}

	libp2pPub, err := crypto.UnmarshalEd25519PublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("chaircall: unmarshal peer key: %w", err)
	}

	id, err := peer.IDFromPublicKey(libp2pPub)
	if err != nil {
		return "", fmt.Errorf("chaircall: derive peer id: %w", err)
	}

	return id, nil
}
