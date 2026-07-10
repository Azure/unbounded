// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBuildSWTPMArgs(t *testing.T) {
	cfg := Config{
		TPMStateDir: "/var/lib/playpen/tpm",
		TPMSocket:   "/run/playpen/swtpm.sock",
	}

	want := []string{
		"socket",
		"--tpm2",
		"--runas", "0",
		"--tpmstate", "dir=/var/lib/playpen/tpm",
		"--ctrl", "type=unixio,path=/run/playpen/swtpm.sock",
		"--terminate",
	}

	got := BuildSWTPMArgs(cfg)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("BuildSWTPMArgs() = %q, want %q", got, want)
	}
}

func TestStartSWTPMAndCloseCleansUpProcessAndSocket(t *testing.T) {
	cfg := testSWTPMConfig(t)
	marker := filepath.Join(t.TempDir(), "terminated")

	var cmd *exec.Cmd

	tpm, err := startSWTPM(
		context.Background(),
		cfg,
		swtpmHelperCommand(t, "ready", cfg.TPMSocket, marker, &cmd),
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("startSWTPM() error = %v", err)
	}

	if err := tpm.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("swtpm helper was not reaped: %#v", cmd.ProcessState)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("swtpm helper did not receive SIGTERM: %v", err)
	}

	if _, err := os.Lstat(cfg.TPMSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TPM socket still exists after Close(): %v", err)
	}

	if err := tpm.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestStartSWTPMReportsEarlyExit(t *testing.T) {
	cfg := testSWTPMConfig(t)

	var cmd *exec.Cmd

	_, err := startSWTPM(
		context.Background(),
		cfg,
		swtpmHelperCommand(t, "fail", cfg.TPMSocket, "", &cmd),
		time.Second,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "swtpm exited before its socket was ready") {
		t.Fatalf("startSWTPM() error = %v, want early exit error", err)
	}

	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("failed swtpm helper was not reaped: %#v", cmd.ProcessState)
	}
}

func TestStartSWTPMReportsStartFailure(t *testing.T) {
	cfg := testSWTPMConfig(t)

	_, err := startSWTPM(
		context.Background(),
		cfg,
		func(_ string, _ ...string) *exec.Cmd {
			return exec.Command(filepath.Join(t.TempDir(), "missing-swtpm"))
		},
		time.Second,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "start swtpm-test-helper") {
		t.Fatalf("startSWTPM() error = %v, want start failure", err)
	}
}

func TestStartSWTPMTimeoutCleansUpProcess(t *testing.T) {
	cfg := testSWTPMConfig(t)
	marker := filepath.Join(t.TempDir(), "terminated")

	var cmd *exec.Cmd

	_, err := startSWTPM(
		context.Background(),
		cfg,
		swtpmHelperCommand(t, "wait", cfg.TPMSocket, marker, &cmd),
		500*time.Millisecond,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("startSWTPM() error = %v, want timeout error", err)
	}

	if cmd.ProcessState == nil {
		t.Fatalf("timed out swtpm helper was not reaped: %#v", cmd.ProcessState)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("timed out swtpm helper did not receive SIGTERM: %v", err)
	}
}

func TestSWTPMCloseKillsUnresponsiveProcess(t *testing.T) {
	cfg := testSWTPMConfig(t)

	var cmd *exec.Cmd

	tpm, err := startSWTPM(
		context.Background(),
		cfg,
		swtpmHelperCommand(t, "ready-ignore-term", cfg.TPMSocket, "", &cmd),
		time.Second,
		50*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("startSWTPM() error = %v", err)
	}

	if err := tpm.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if cmd.ProcessState == nil {
		t.Fatal("unresponsive swtpm helper was not reaped")
	}

	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("unresponsive swtpm helper was not killed and reaped: %#v", cmd.ProcessState)
	}

	if _, err := os.Lstat(cfg.TPMSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TPM socket still exists after forced cleanup: %v", err)
	}
}

func TestStartSWTPMRejectsNonSocketWithoutDeletingIt(t *testing.T) {
	cfg := testSWTPMConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.TPMSocket), 0o755); err != nil {
		t.Fatalf("create TPM socket directory: %v", err)
	}

	if err := os.WriteFile(cfg.TPMSocket, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write TPM socket path: %v", err)
	}

	called := false

	_, err := startSWTPM(
		context.Background(),
		cfg,
		func(name string, args ...string) *exec.Cmd {
			called = true

			return exec.Command(name, args...)
		},
		time.Second,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "is not a Unix socket") {
		t.Fatalf("startSWTPM() error = %v, want non-socket error", err)
	}

	if called {
		t.Fatal("swtpm command was started with an unsafe socket path")
	}

	data, readErr := os.ReadFile(cfg.TPMSocket)
	if readErr != nil || string(data) != "keep" {
		t.Fatalf("non-socket path was changed: data %q error %v", data, readErr)
	}
}

func TestSWTPMHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SWTPM_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("SWTPM_HELPER_MODE")
	socketPath := os.Getenv("SWTPM_HELPER_SOCKET")
	marker := os.Getenv("SWTPM_HELPER_MARKER")

	if mode == "fail" {
		os.Exit(23)
	}

	if mode == "ready-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}

	signals := make(chan os.Signal, 1)
	if mode != "ready-ignore-term" {
		signal.Notify(signals, syscall.SIGTERM)
	}

	var listener net.Listener

	if mode == "ready" || mode == "ready-ignore-term" {
		var err error

		listener, err = net.Listen("unix", socketPath)
		if err != nil {
			os.Exit(24)
		}
	}

	if mode == "ready-ignore-term" {
		select {}
	}

	<-signals

	if listener != nil {
		_ = listener.Close()
	}

	if marker != "" {
		_ = os.WriteFile(marker, []byte("terminated"), 0o600)
	}

	os.Exit(0)
}

func testSWTPMConfig(t *testing.T) Config {
	t.Helper()

	dir := t.TempDir()

	return Config{
		SWTPMBinary: "swtpm-test-helper",
		TPMStateDir: filepath.Join(dir, "state"),
		TPMSocket:   filepath.Join(dir, "run", "swtpm.sock"),
	}
}

func swtpmHelperCommand(
	t *testing.T,
	mode string,
	socketPath string,
	marker string,
	gotCmd **exec.Cmd,
) commandBuilder {
	t.Helper()

	return func(_ string, _ ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSWTPMHelperProcess$")

		cmd.Env = append(os.Environ(),
			"GORACE=atexit_sleep_ms=0",
			"GO_WANT_SWTPM_HELPER_PROCESS=1",
			"SWTPM_HELPER_MODE="+mode,
			"SWTPM_HELPER_SOCKET="+socketPath,
			"SWTPM_HELPER_MARKER="+marker,
		)
		*gotCmd = cmd

		return cmd
	}
}
