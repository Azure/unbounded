// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"time"
)

func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159)) }

func makeCA(now time.Time, o Options) (authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return authority{}, err
	}

	n, err := serial()
	if err != nil {
		return authority{}, err
	}

	template := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "Racer root CA"}, NotBefore: now.Add(-o.ClockSkew), NotAfter: now.Add(o.CALifetime), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return authority{}, err
	}

	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return authority{}, err
	}

	return authority{Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})), Digest: digest(der)}, nil
}

func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate

	for len(bytes.TrimSpace(data)) > 0 {
		data = bytes.TrimSpace(data)
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("invalid certificate PEM")
		}

		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("invalid certificate PEM")
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		certs = append(certs, cert)
		data = rest
	}

	if len(certs) == 0 {
		return nil, errors.New("empty certificates")
	}

	return certs, nil
}

func parseAuthority(ca authority) (*x509.Certificate, crypto.Signer, error) {
	certs, err := parseCertificates([]byte(ca.Certificate))
	if err != nil {
		return nil, nil, err
	}

	if len(certs) != 1 || digest(certs[0].Raw) != ca.Digest || !certs[0].IsCA || certs[0].CheckSignatureFrom(certs[0]) != nil {
		return nil, nil, errors.New("invalid persisted CA")
	}

	block, rest := pem.Decode([]byte(ca.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("invalid persisted private key")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("CA key cannot sign")
	}

	pub, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, nil, err
	}

	if !bytes.Equal(pub, certs[0].RawSubjectPublicKeyInfo) {
		return nil, nil, errors.New("CA certificate/key mismatch")
	}

	return certs[0], signer, nil
}

func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()

	if err := d.Decode(v); err != nil {
		return err
	}

	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}

	return nil
}

// ParseBundle rejects malformed, duplicate, non-root, or excessive trust anchors.
func ParseBundle(data []byte) (TrustBundle, error) {
	var b TrustBundle
	if err := strictJSON(data, &b); err != nil {
		return b, err
	}

	if b.Version != 1 || b.Generation == 0 || !hexID.MatchString(b.Active) {
		return b, errors.New("invalid trust bundle metadata")
	}

	certs, err := parseCertificates([]byte(b.Certificates))
	if err != nil {
		return b, err
	}

	if len(certs) > 2 {
		return b, errors.New("more than two trust anchors")
	}

	seen := map[string]bool{}

	for _, cert := range certs {
		id := digest(cert.Raw)
		if seen[id] || !cert.IsCA || !cert.BasicConstraintsValid || cert.CheckSignatureFrom(cert) != nil {
			return b, errors.New("invalid or duplicate trust anchor")
		}

		seen[id] = true
	}

	if !seen[b.Active] {
		return b, errors.New("active root absent from trust bundle")
	}
	// Publication uses these exact bytes. Reject alternate encodings rather than
	// acknowledging a canonicalized digest that differs from the mounted file.
	if !bytes.Equal(data, b.JSON()) {
		return b, errors.New("trust bundle must use the exact canonical publication encoding")
	}

	return b, nil
}

func parseCSR(data []byte) (*x509.CertificateRequest, error) {
	if block, rest := pem.Decode(data); block != nil {
		if block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("invalid CSR PEM")
		}

		data = block.Bytes
	}

	csr, err := x509.ParseCertificateRequest(data)
	if err != nil {
		return nil, err
	}

	if err = csr.CheckSignature(); err != nil {
		return nil, err
	}

	switch key := csr.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return nil, errors.New("RSA key must have at least 2048 bits")
		}
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
			return nil, errors.New("unsupported elliptic curve")
		}
	case ed25519.PublicKey:
	default:
		return nil, errors.New("unsupported CSR public key")
	}

	return csr, nil
}

func signLeaf(ca authority, csr *x509.CertificateRequest, identity Identity, namespace string, now time.Time, o Options) ([]byte, time.Time, error) {
	parent, key, err := parseAuthority(ca)
	if err != nil {
		return nil, time.Time{}, err
	}

	uri, err := identityURL(identity)
	if err != nil {
		return nil, time.Time{}, err
	}

	n, err := serial()
	if err != nil {
		return nil, time.Time{}, err
	}

	expiry := now.Add(o.LeafLifetime).Truncate(time.Second)
	if now.Before(parent.NotBefore) || !expiry.Add(o.ClockSkew).Before(parent.NotAfter) {
		return nil, time.Time{}, errors.New("CA cannot cover requested leaf lifetime")
	}
	// Deliberately construct a new template: CSR identities and all requested
	// extensions (including CA constraints and EKUs) are ignored.
	template := &x509.Certificate{SerialNumber: n, NotBefore: now.Add(-o.ClockSkew), NotAfter: expiry, URIs: []*url.URL{uri}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if identity.Kind == Node {
		template.ExtKeyUsage = append(template.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	} else {
		template.DNSNames = []string{fmt.Sprintf("racer-controlplane.%s.svc", namespace)}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, parent, csr.PublicKey, key)
	if err != nil {
		return nil, time.Time{}, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), expiry, nil
}
