// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRustKeyringInterop runs the production Rust transport against the real Go
// HTTPS handler, issuer, and keyring reconciler with the existing fake API fixture.
// Opt in with RACER_RUST_INTEROP=1; no Kubernetes cluster is contacted.
func TestRustKeyringInterop(t *testing.T) {
	if os.Getenv("RACER_RUST_INTEROP") != "1" {
		t.Skip("set RACER_RUST_INTEROP=1 to run the Rust client")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	directory, err := os.MkdirTemp(filepath.Join(root, "tmp"), "keyring-interop-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	f := newServingFixture(t)

	cache := catalogCache("interop", testOtherUID)
	if err := f.a.Topology.Create(f.ctx, &cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, f.a.Keyring)
	reconcileTopology(t, f.a.Topology, f.ctx)
	endpoint := f.start(t)

	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.serverCertificate.Certificate[0]})
	if err := os.WriteFile(filepath.Join(directory, "trust.pem"), trust, 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := json.Marshal(map[string]string{
		"endpoint": endpoint,
		"cluster":  string(f.a.Server.Config.Cluster),
		"node":     testNodeUID,
		"token":    f.token,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(directory, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "timeout", "--signal=TERM", "--kill-after=10s", "230s", "cargo", "test", "--locked", "--manifest-path", filepath.Join(root, "cmd/racer-dataplane/Cargo.toml"), "--test", "keyring_interop", "--", "--ignored", "--nocapture")

	cmd.Env = append(os.Environ(), "RACER_KEYRING_INTEROP_DIR="+directory)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	reaped := false

	defer func() {
		if !reaped {
			cancel()
			<-done
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	// No manager runs in this fixture. Keep authority observations fresh through
	// Cargo startup and the full no-change poll without relaxing freshness gates.
	refresh := time.NewTicker(time.Second)
	defer refresh.Stop()

	rotated := false

	for {
		select {
		case err := <-done:
			reaped = true

			if err != nil {
				t.Fatalf("Rust interoperability client: %v", err)
			}

			if !rotated {
				t.Fatal("Rust client did not reach the rotation poll")
			}

			return
		case <-refresh.C:
			runKeys(t, f.a.Keyring)
			reconcileTopology(t, f.a.Topology, f.ctx)
		case <-ticker.C:
			if rotated {
				continue
			}

			if _, err := os.Stat(filepath.Join(directory, "rotate")); err != nil {
				continue
			}

			parked := f.a.Server.keyringPolls.count() == 1

			if parked {
				_, _, rotation, _ := keyState(t, f.a.Keyring)
				fixtureDependencies[f.a.authority].now = func() time.Time { return rotation.NextRotation }
				runKeys(t, f.a.Keyring)

				rotated = true
			}
		}
	}
}
