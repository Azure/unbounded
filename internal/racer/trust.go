// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
)

// TrustBundle is the public bundle.json contract shared with the Rust control
// plane. It contains no issuance, private persistence, or rotation machinery.
type TrustBundle struct {
	Version      int    `json:"version"`
	Generation   uint64 `json:"generation"`
	Active       string `json:"active"`
	Certificates string `json:"certificates"`
}

func (b TrustBundle) JSON() []byte {
	data, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}

	return data
}

func (b TrustBundle) Digest() string {
	sum := sha256.Sum256(b.JSON())
	return hex.EncodeToString(sum[:])
}

// ParseTrustBundle validates the roots and requires exact canonical bytes so a
// digest always identifies the published file, never a normalized substitute.
func ParseTrustBundle(data []byte) (TrustBundle, error) {
	var bundle TrustBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return bundle, err
	}

	if bundle.Version != 1 || bundle.Generation == 0 || !bytes.Equal(data, bundle.JSON()) {
		return bundle, fmt.Errorf("invalid or noncanonical trust bundle")
	}

	seen := map[string]bool{}

	remaining := []byte(bundle.Certificates)
	for len(bytes.TrimSpace(remaining)) > 0 {
		remaining = bytes.TrimSpace(remaining)
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return bundle, fmt.Errorf("invalid root PEM")
		}

		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return bundle, fmt.Errorf("invalid root PEM")
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return bundle, err
		}

		sum := sha256.Sum256(cert.Raw)

		id := hex.EncodeToString(sum[:])
		if seen[id] || !cert.IsCA || !cert.BasicConstraintsValid || cert.CheckSignatureFrom(cert) != nil {
			return bundle, fmt.Errorf("invalid or duplicate trust root")
		}

		seen[id] = true
		remaining = rest
	}

	if len(seen) == 0 || len(seen) > 2 || !seen[bundle.Active] {
		return bundle, fmt.Errorf("invalid trust roots or absent active root")
	}

	return bundle, nil
}
