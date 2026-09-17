// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureNodeExporter(t *testing.T) {
	t.Parallel()

	const version = "1.9.1"

	machineDir := t.TempDir()
	artifactDir := t.TempDir()
	archivePath := filepath.Join(artifactDir, "node_exporter-1.9.1.linux-amd64.tar.gz")

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	gzipWriter := gzip.NewWriter(archiveFile)
	tarWriter := tar.NewWriter(gzipWriter)

	binary := []byte("#!/bin/sh\necho 'node_exporter, version 1.9.1'\n")
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "node_exporter-1.9.1.linux-amd64/node_exporter",
		Mode: 0o755,
		Size: int64(len(binary)),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}

	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}

	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(archiveData)

	checksumPath := archivePath + ".sha256"
	if err := os.WriteFile(checksumPath, []byte(hex.EncodeToString(digest[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	goal := &goalstates.RootFS{
		MachineDir: machineDir,
		HostArch:   "amd64",
		Downloads: &goalstates.DownloadOverrides{NodeExporter: &goalstates.DownloadSource{
			URL: filepath.Join(artifactDir, "node_exporter-%[1]s.linux-%[2]s.tar.gz"),
		}},
		NodeExporter: goalstates.NodeExporter{
			Enabled:       true,
			Version:       version,
			ListenAddress: "10.0.0.4:9100",
		},
	}
	if err := ConfigureNodeExporter(slog.New(slog.DiscardHandler), goal).Do(t.Context()); err != nil {
		t.Fatalf("ConfigureNodeExporter() error = %v", err)
	}

	for _, path := range []string{
		"usr/local/bin/node_exporter",
		"etc/systemd/system/node-exporter.service",
		"etc/systemd/system/multi-user.target.wants/node-exporter.service",
	} {
		if _, err := os.Lstat(filepath.Join(machineDir, path)); err != nil {
			t.Errorf("expected installed path %s: %v", path, err)
		}
	}
}

func TestRenderNodeExporterService(t *testing.T) {
	t.Parallel()

	content := string(renderNodeExporterService(goalstates.NodeExporter{
		Enabled:       true,
		ListenAddress: "10.0.0.4:19100",
		ExtraArgs:     []string{`--collector.filesystem.mount-points-exclude=^/(dev|proc)($|/)`},
		TLS:           goalstates.NodeExporterTLS{Enabled: true},
	}))
	for _, expected := range []string{
		`--web.listen-address=10.0.0.4:19100`,
		`--web.config.file=/etc/unbounded/node-exporter/web-config.yml`,
		`--collector.filesystem.mount-points-exclude=^/(dev|proc)($$|/)`,
		`Restart=on-failure`,
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("renderNodeExporterService() missing %q:\n%s", expected, content)
		}
	}
}

func TestRenderNodeExporterWebConfig(t *testing.T) {
	t.Parallel()

	content := string(renderNodeExporterWebConfig(goalstates.NodeExporterTLS{
		Enabled:         true,
		CertificateFile: "/etc/node-exporter/tls.crt",
		PrivateKeyFile:  "/etc/node-exporter/tls.key",
		ClientCAFile:    "/etc/node-exporter/ca.crt",
	}))
	for _, expected := range []string{
		`cert_file: "/etc/node-exporter/tls.crt"`,
		`key_file: "/etc/node-exporter/tls.key"`,
		`client_auth_type: RequireAndVerifyClientCert`,
		`client_ca_file: "/etc/node-exporter/ca.crt"`,
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("renderNodeExporterWebConfig() missing %q:\n%s", expected, content)
		}
	}
}

func TestNodeExporterExpectedSHA256(t *testing.T) {
	t.Parallel()

	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	dir := t.TempDir()

	manifestPath := filepath.Join(dir, "sha256sums.txt")
	if err := os.WriteFile(manifestPath, []byte(digest+"  node_exporter-1.9.1.linux-amd64.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	source, err := artifactsource.Parse(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	got, err := nodeExporterExpectedSHA256(t.Context(), source, "node_exporter-1.9.1.linux-amd64.tar.gz")
	if err != nil {
		t.Fatalf("nodeExporterExpectedSHA256() error = %v", err)
	}

	if got != digest {
		t.Fatalf("nodeExporterExpectedSHA256() = %q, want %q", got, digest)
	}
}
