// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chaircall

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func testIdentity(t *testing.T) (crypto.PrivKey, peer.ID) {
	t.Helper()

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("IDFromPrivateKey: %v", err)
	}

	return priv, id
}

// handshake runs a TLS handshake between a listener using serverPriv and a
// client pinning expected, returning the client-side error.
func handshake(t *testing.T, serverPriv crypto.PrivKey, expected peer.ID) error {
	t.Helper()

	serverCfg, err := ServerTLSConfig(serverPriv)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() }) //nolint:errcheck // test cleanup

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		defer conn.Close() //nolint:errcheck // test cleanup

		_, _ = io.Copy(io.Discard, conn) //nolint:errcheck // draining
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), ClientTLSConfig(expected))
	if err != nil {
		return err
	}

	defer conn.Close() //nolint:errcheck // test cleanup

	return conn.Handshake()
}

func TestPinnedTLSAcceptsMatchingPeer(t *testing.T) {
	priv, id := testIdentity(t)

	if err := handshake(t, priv, id); err != nil {
		t.Fatalf("handshake with matching peer id failed: %v", err)
	}
}

// The pin is the entire trust decision, so a certificate from any other
// identity must be refused even though it is otherwise well formed.
func TestPinnedTLSRejectsDifferentPeer(t *testing.T) {
	serverPriv, serverID := testIdentity(t)
	_, otherID := testIdentity(t)

	err := handshake(t, serverPriv, otherID)
	if err == nil {
		t.Fatal("handshake succeeded against an unexpected peer id")
	}

	if !strings.Contains(err.Error(), otherID.String()) && !strings.Contains(err.Error(), serverID.String()) {
		t.Fatalf("error does not identify the peer mismatch: %v", err)
	}
}

func TestPeerIDFromCertificateRoundTrips(t *testing.T) {
	priv, id := testIdentity(t)

	cfg, err := ServerTLSConfig(priv)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	if len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		t.Fatal("server config carries no certificate")
	}

	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	got, err := PeerIDFromCertificate(leaf)
	if err != nil {
		t.Fatalf("PeerIDFromCertificate: %v", err)
	}

	if got != id {
		t.Fatalf("peer id = %s, want %s", got, id)
	}
}

func TestServerTLSConfigPinsALPNAndTLS13(t *testing.T) {
	priv, _ := testIdentity(t)

	cfg, err := ServerTLSConfig(priv)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
	}

	if len(cfg.NextProtos) == 0 || cfg.NextProtos[0] != "h2" {
		t.Fatalf("NextProtos = %v, want h2 first", cfg.NextProtos)
	}
}
