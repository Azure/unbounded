// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureLocalDNS(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()

	binary := filepath.Join(sourceDir, "1.12.3", "amd64", "coredns")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}

	plugins := "bind\ncache\nerrors\nforward\nloop\nprometheus\nready\nwhoami\n"

	script := "#!/bin/sh\nprintf '" + strings.ReplaceAll(plugins, "\n", "\\n") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte(script))
	if err := os.WriteFile(binary+".sha256", []byte(fmt.Sprintf("%x  coredns\n", digest)), 0o644); err != nil {
		t.Fatal(err)
	}

	machineDir := t.TempDir()

	goal := &goalstates.RootFS{
		MachineDir: machineDir,
		HostArch:   "amd64",
		Downloads: &goalstates.DownloadOverrides{CoreDNS: &goalstates.DownloadSource{
			URL:     "file://" + sourceDir + "/%s/%s/coredns",
			Version: "1.12.3",
		}},
		LocalDNS: goalstates.LocalDNS{
			Enabled:              true,
			CoreDNSVersion:       "1.12.3",
			NodeListenerIP:       mustAddr(t, "169.254.10.10"),
			ClusterListenerIP:    mustAddr(t, "169.254.10.11"),
			NodeUpstreamIPs:      []netip.Addr{mustAddr(t, "10.0.0.4")},
			CPULimitInMilliCores: 2000,
			MemoryLimitInMB:      128,
			RequiredPlugins:      strings.Fields(plugins),
			Corefile:             []byte(".:53 { bind 169.254.10.10 }"),
		},
	}
	if err := ConfigureLocalDNS(slog.New(slog.DiscardHandler), goal).Do(t.Context()); err != nil {
		t.Fatalf("ConfigureLocalDNS() error = %v", err)
	}

	for _, path := range []string{
		"usr/local/bin/coredns",
		"usr/local/libexec/unbounded-localdns-supervisor",
		"etc/unbounded/localdns/Corefile",
		"etc/unbounded/localdns/node-upstreams",
		"etc/systemd/system/localdns.service",
		"etc/systemd/system/localdns.slice",
		"etc/systemd/system/multi-user.target.wants/localdns.service",
	} {
		if _, err := os.Stat(filepath.Join(machineDir, path)); err != nil {
			t.Errorf("expected %s: %v", path, err)
		}
	}

	slice, err := os.ReadFile(filepath.Join(machineDir, "etc/systemd/system/localdns.slice"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(slice), "CPUQuota=200%") {
		t.Fatalf("localdns.slice missing percentage CPU quota:\n%s", slice)
	}
}

func TestLocalDNSResolvConfRemovesAllNameservers(t *testing.T) {
	t.Parallel()

	original := []byte("search example.test\n nameserver 10.0.0.4\nnameserver\t127.0.0.53\noptions timeout:2\n")
	got := string(localDNSResolvConf(original, "169.254.10.10"))
	want := "search example.test\noptions timeout:2\nnameserver 169.254.10.10\n"

	if got != want {
		t.Fatalf("localDNSResolvConf() = %q, want %q", got, want)
	}
}

func mustAddr(t *testing.T, value string) netip.Addr {
	t.Helper()

	addr, err := netip.ParseAddr(value)
	if err != nil {
		t.Fatal(err)
	}

	return addr
}
