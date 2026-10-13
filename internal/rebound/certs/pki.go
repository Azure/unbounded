// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package certs manages rebound's serving certificates and trust bundle.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"
)

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("missing PEM certificate")
	}

	return x509.ParseCertificate(block.Bytes)
}

func loadPair(cert, key []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, nil, err
	}

	privateKey, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || privateKey.Curve != elliptic.P256() {
		return nil, nil, fmt.Errorf("expected ECDSA P-256 key")
	}

	parsed, err := x509.ParseCertificate(pair.Certificate[0])

	return parsed, privateKey, err
}

func loadCA(cert, key []byte, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	parsed, privateKey, err := loadPair(cert, key)
	if err != nil {
		return nil, nil, err
	}

	if !parsed.IsCA || now.Before(parsed.NotBefore) || !now.Before(parsed.NotAfter) {
		return nil, nil, fmt.Errorf("CA is not valid now")
	}

	if err := parsed.CheckSignatureFrom(parsed); err != nil {
		return nil, nil, err
	}

	return parsed, privateKey, nil
}

func issue(now time.Time, lifetime time.Duration, names []string, ca *x509.Certificate, signer *ecdsa.PrivateKey) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rebound"},
		NotBefore:             now.UTC().Truncate(time.Second),
		NotAfter:              now.Add(lifetime).UTC().Truncate(time.Second),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if ca == nil {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
		template.MaxPathLenZero = true
		ca, signer = template, key
	} else {
		template.NotAfter = earlier(template.NotAfter, ca.NotAfter)
		template.DNSNames = names
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func validLeaf(cert *x509.Certificate, ca *x509.Certificate, names []string, now time.Time) bool {
	if cert.IsCA || now.Before(cert.NotBefore) || !now.Before(renewAt(cert)) || cert.CheckSignatureFrom(ca) != nil {
		return false
	}

	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || len(cert.DNSNames) != len(names) {
		return false
	}

	for _, name := range names {
		if !slices.Contains(cert.DNSNames, name) {
			return false
		}
	}

	return cert.NotAfter.Compare(ca.NotAfter) <= 0
}

func renewAt(cert *x509.Certificate) time.Time {
	return cert.NotBefore.Add(cert.NotAfter.Sub(cert.NotBefore) * 2 / 3)
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}
