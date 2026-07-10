// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureBMCCertificatePersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BMCCertPath: filepath.Join(dir, "redfish.crt"),
		BMCKeyPath:  filepath.Join(dir, "redfish.key"),
	}

	if err := ensureBMCCertificate(cfg); err != nil {
		t.Fatalf("ensureBMCCertificate() error = %v", err)
	}

	first, err := tls.LoadX509KeyPair(cfg.BMCCertPath, cfg.BMCKeyPath)
	if err != nil {
		t.Fatalf("load first key pair: %v", err)
	}

	if err := ensureBMCCertificate(cfg); err != nil {
		t.Fatalf("second ensureBMCCertificate() error = %v", err)
	}

	second, err := tls.LoadX509KeyPair(cfg.BMCCertPath, cfg.BMCKeyPath)
	if err != nil {
		t.Fatalf("load second key pair: %v", err)
	}

	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Fatal("BMC certificate changed on second initialization")
	}

	info, err := os.Stat(cfg.BMCKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("BMC key mode = %o, want 600", info.Mode().Perm())
	}
}
