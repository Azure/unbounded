// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const certificateTypePEM = "PEM"

// SecureBootCertificate holds certificate metadata reported by Redfish.
type SecureBootCertificate struct {
	CertificateString        string
	Fingerprint              string
	FingerprintHashAlgorithm string
}

// SecureBootCertificateMaterial is validated certificate material ready for enrollment.
type SecureBootCertificateMaterial struct {
	PEM               string
	SHA256Fingerprint string
}

// ParseSecureBootCertificate validates a PEM-encoded X.509 certificate and
// returns the Redfish-compatible SHA-256 fingerprint over its DER bytes.
func ParseSecureBootCertificate(certPEM string) (SecureBootCertificateMaterial, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return SecureBootCertificateMaterial{}, fmt.Errorf("decoding PEM certificate: no PEM block found")
	}

	if block.Type != "CERTIFICATE" {
		return SecureBootCertificateMaterial{}, fmt.Errorf("decoding PEM certificate: got PEM block type %q", block.Type)
	}

	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return SecureBootCertificateMaterial{}, fmt.Errorf("parsing X.509 certificate: %w", err)
	}

	h := sha256.Sum256(block.Bytes)

	return SecureBootCertificateMaterial{
		PEM:               certPEM,
		SHA256Fingerprint: formatFingerprint(h[:]),
	}, nil
}

// ListSecureBootCertificates returns certificates in a UEFI Secure Boot database.
// Returns ErrUnsupported if the BMC does not expose certificate enrollment.
func (c *Client) ListSecureBootCertificates(ctx context.Context, database string) ([]SecureBootCertificate, error) {
	path := c.secureBootCertificateCollectionPath(database)

	data, status, err := c.session.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	if isUnsupportedStatus(status) {
		return nil, fmt.Errorf("SecureBoot certificate collection GET returned %d: %w", status, ErrUnsupported)
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var collection struct {
		Members []struct {
			ODataID                  string `json:"@odata.id"`
			CertificateString        string `json:"CertificateString"`
			Fingerprint              string `json:"Fingerprint"`
			FingerprintHashAlgorithm string `json:"FingerprintHashAlgorithm"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(data, &collection); err != nil {
		return nil, fmt.Errorf("parsing SecureBoot certificate collection: %w", err)
	}

	certs := make([]SecureBootCertificate, 0, len(collection.Members))
	for _, member := range collection.Members {
		cert := SecureBootCertificate{
			CertificateString:        member.CertificateString,
			Fingerprint:              member.Fingerprint,
			FingerprintHashAlgorithm: member.FingerprintHashAlgorithm,
		}

		if cert.CertificateString == "" && cert.Fingerprint == "" && member.ODataID != "" {
			fetched, err := c.getSecureBootCertificate(ctx, member.ODataID)
			if err != nil {
				return nil, err
			}

			cert = fetched
		}

		certs = append(certs, cert)
	}

	return certs, nil
}

// InstallSecureBootCertificate enrolls a PEM certificate into a UEFI Secure Boot database.
// Returns ErrUnsupported if the BMC does not support certificate enrollment.
func (c *Client) InstallSecureBootCertificate(ctx context.Context, database string, cert SecureBootCertificateMaterial) error {
	path := c.secureBootCertificateCollectionPath(database)
	body := map[string]string{
		"CertificateString": cert.PEM,
		"CertificateType":   certificateTypePEM,
	}

	data, status, err := c.session.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return fmt.Errorf("SecureBoot certificate collection POST returned %d: %w", status, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return fmt.Errorf("unexpected status %d from SecureBoot certificate collection POST: %s", status, data)
	}

	return nil
}

func (c *Client) getSecureBootCertificate(ctx context.Context, path string) (SecureBootCertificate, error) {
	path, err := redfishResourcePath(path)
	if err != nil {
		return SecureBootCertificate{}, err
	}

	data, status, err := c.session.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return SecureBootCertificate{}, err
	}

	if isUnsupportedStatus(status) {
		return SecureBootCertificate{}, fmt.Errorf("SecureBoot certificate GET returned %d: %w", status, ErrUnsupported)
	}

	if status != http.StatusOK {
		return SecureBootCertificate{}, fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var cert struct {
		CertificateString        string `json:"CertificateString"`
		Fingerprint              string `json:"Fingerprint"`
		FingerprintHashAlgorithm string `json:"FingerprintHashAlgorithm"`
	}
	if err := json.Unmarshal(data, &cert); err != nil {
		return SecureBootCertificate{}, fmt.Errorf("parsing SecureBoot certificate: %w", err)
	}

	return SecureBootCertificate{
		CertificateString:        cert.CertificateString,
		Fingerprint:              cert.Fingerprint,
		FingerprintHashAlgorithm: cert.FingerprintHashAlgorithm,
	}, nil
}

func (c *Client) secureBootCertificateCollectionPath(database string) string {
	return fmt.Sprintf("/redfish/v1/Systems/%s/SecureBoot/SecureBootDatabases/%s/Certificates", c.deviceID, database)
}

func secureBootCertificateInstalled(existing []SecureBootCertificate, desired SecureBootCertificateMaterial) bool {
	desiredFingerprint := normalizeFingerprint(desired.SHA256Fingerprint)

	for _, cert := range existing {
		if isSHA256FingerprintAlgorithm(cert.FingerprintHashAlgorithm) && normalizeFingerprint(cert.Fingerprint) == desiredFingerprint {
			return true
		}

		if cert.CertificateString == "" {
			continue
		}

		parsed, err := ParseSecureBootCertificate(cert.CertificateString)
		if err == nil && normalizeFingerprint(parsed.SHA256Fingerprint) == desiredFingerprint {
			return true
		}
	}

	return false
}

func isSHA256FingerprintAlgorithm(algorithm string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(algorithm, "-", ""), "_", ""))

	return normalized == "SHA256"
}

func normalizeFingerprint(fingerprint string) string {
	replacer := strings.NewReplacer(":", "", " ", "", "-", "")

	return strings.ToLower(replacer.Replace(fingerprint))
}

func redfishResourcePath(resourceID string) (string, error) {
	if !strings.HasPrefix(resourceID, "http://") && !strings.HasPrefix(resourceID, "https://") {
		return resourceID, nil
	}

	u, err := url.Parse(resourceID)
	if err != nil {
		return "", fmt.Errorf("parsing Redfish resource URI %q: %w", resourceID, err)
	}

	if u.Path == "" {
		return "", fmt.Errorf("redfish resource URI %q has no path", resourceID)
	}

	return u.Path, nil
}
