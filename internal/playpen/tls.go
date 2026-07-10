// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func ensureBMCCertificate(cfg Config) error {
	certExists, err := regularFileExists(cfg.BMCCertPath)
	if err != nil {
		return err
	}

	keyExists, err := regularFileExists(cfg.BMCKeyPath)
	if err != nil {
		return err
	}

	if certExists || keyExists {
		if !certExists || !keyExists {
			return fmt.Errorf("BMC TLS certificate and key must either both exist or both be absent")
		}

		if _, err := tls.LoadX509KeyPair(cfg.BMCCertPath, cfg.BMCKeyPath); err != nil {
			return fmt.Errorf("load BMC TLS certificate: %w", err)
		}

		return nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate BMC TLS key: %w", err)
	}

	now := time.Now()

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate BMC TLS serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "playpen-bmc"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create BMC TLS certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := writeFileExclusive(cfg.BMCCertPath, certPEM, 0o644); err != nil {
		return err
	}

	if err := writeFileExclusive(cfg.BMCKeyPath, keyPEM, 0o600); err != nil {
		return errors.Join(err, removeRegularFile(cfg.BMCCertPath))
	}

	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}

	return true, nil
}

func writeFileExclusive(path string, data []byte, mode os.FileMode) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close %s: %w", path, err))
		}
	}()

	if _, err := file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), removeRegularFile(path))
	}

	return nil
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("stat %s for cleanup: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular file %s", path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}
