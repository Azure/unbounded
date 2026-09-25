// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}

	return bytes.TrimSuffix(b, []byte{'\n'})
}

func TestSharedPublicationVector(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	if err != nil {
		t.Fatal(err)
	}

	p, m, err := CanonicalContent(v)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(p, fixture(t, "content.json")) || !bytes.Equal(m, fixture(t, "membership.json")) {
		t.Fatalf("canonical mismatch\n%s\n%s", p, m)
	}

	ph, mh, err := ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	var hashes struct{ Content, Membership string }
	if err := json.Unmarshal(fixture(t, "hashes.json"), &hashes); err != nil {
		t.Fatal(err)
	}

	if ph != hashes.Content || mh != hashes.Membership {
		t.Fatal("hash mismatch")
	}

	encoded, err := EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := DecodePublication(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	again, err := EncodePublication(replay)
	if err != nil || !bytes.Equal(encoded, again) {
		t.Fatal("unstable round trip", err)
	}
}

// Generate public test artifacts in memory. The key is ephemeral and never
// printed or persisted. The wire validates DER syntax, not trust or CSR authority.
func publicDER(t *testing.T) (cert, csr []byte) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wire fixture"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(2000000000, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}

	cert, err = x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	csr, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: template.Subject}, key)
	if err != nil {
		t.Fatal(err)
	}

	return cert, csr
}

func TestPublicDEREncoding(t *testing.T) {
	cert, csr := publicDER(t)
	cluster := ClusterID("11111111-1111-4111-8111-111111111111")
	enrollment := EnrollmentID("55555555-5555-4555-8555-555555555555")

	request, err := EncodeBootstrapRequest(BootstrapRequest{SchemaVersion: 1, Cluster: cluster, Enrollment: enrollment, CSRDER: csr})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeBootstrap(bytes.NewReader(request)); err != nil {
		t.Fatal(err)
	}

	response, err := EncodeBootstrap(BootstrapResponse{SchemaVersion: 1, Cluster: cluster, Enrollment: enrollment, Node: "22222222-2222-4222-8222-222222222222", CertificateChain: [][]byte{cert}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeBootstrapResponse(bytes.NewReader(response)); err != nil {
		t.Fatal(err)
	}

	if _, err := json.Marshal(KeyringBundle{}); err == nil {
		t.Fatal("invalid bundle accepted")
	}
}
