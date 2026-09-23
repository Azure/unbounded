// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func trustRoot(t *testing.T, ca bool) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(der)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), hex.EncodeToString(sum[:])
}

func TestPublicTrustBundle(t *testing.T) {
	root, id := trustRoot(t, true)
	next, nextID := trustRoot(t, true)
	third, _ := trustRoot(t, true)
	leaf, leafID := trustRoot(t, false)

	for _, bundle := range []TrustBundle{
		{Version: 1, Generation: 1, Active: id, Certificates: root},
		{Version: 1, Generation: 2, Active: id, Certificates: root + next},
		{Version: 1, Generation: 3, Active: nextID, Certificates: root + next},
		{Version: 1, Generation: 4, Active: nextID, Certificates: next},
	} {
		wire := bundle.JSON()

		parsed, err := ParseTrustBundle(wire)
		if err != nil || parsed != bundle {
			t.Fatalf("rotation publication: %v", err)
		}

		sum := sha256.Sum256(wire)
		if parsed.Digest() != hex.EncodeToString(sum[:]) {
			t.Fatal("digest differs from exact published bytes")
		}

		for _, malformed := range [][]byte{append(append([]byte(nil), wire...), '\n'), append(append([]byte(nil), wire...), wire...), append([]byte(`{"extra":1,`), wire[1:]...)} {
			if _, err := ParseTrustBundle(malformed); err == nil {
				t.Fatal("accepted alternate or malformed publication")
			}
		}
	}

	for name, bundle := range map[string]TrustBundle{
		"version":        {Version: 4, Generation: 1, Active: id, Certificates: root},
		"generation":     {Version: 1, Active: id, Certificates: root},
		"missing-active": {Version: 1, Generation: 1, Active: nextID, Certificates: root},
		"empty":          {Version: 1, Generation: 1, Active: id},
		"duplicate":      {Version: 1, Generation: 1, Active: id, Certificates: root + root},
		"three":          {Version: 1, Generation: 1, Active: id, Certificates: root + next + third},
		"non-ca":         {Version: 1, Generation: 1, Active: leafID, Certificates: leaf},
		"junk":           {Version: 1, Generation: 1, Active: id, Certificates: "junk" + root},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTrustBundle(bundle.JSON()); err == nil {
				t.Fatal("accepted invalid trust")
			}
		})
	}
}
