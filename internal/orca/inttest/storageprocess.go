// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest && storageboundary

package inttest

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// storageprocess.go orchestrates a two-node unbounded-storage ring as
// out-of-process child binaries for the boundary test. It is the Go
// port of the process-management half of hack/smoke-storage.py: it
// raises RLIMIT_MEMLOCK (io_uring registers fixed buffers that must be
// pinned), allocates loopback ports, writes per-node TOML configs whose
// S3 backend points at an orca edge listener, spawns the binaries in
// their own process groups (teeing output to per-node log files), and
// waits for both frontend ports to accept connections. Cleanup kills
// each process group on t.Cleanup.

const (
	// storagePageSize is the unbounded-storage disk page size. "A single
	// page" in the test's key-size matrix refers to this value.
	storagePageSize = 4096

	// storageStripeSize is the backend stripe (cache line) size. The
	// 1 GiB object spans 256 of these, forcing the multi-stripe stream
	// + eviction + cross-node fetch paths.
	storageStripeSize = 4 * 1024 * 1024

	// storageDiskSize is the per-node file-backed disk size in bytes. It
	// must be a multiple of the page size and large enough to hold every
	// stripe of the largest object on one node. The proto3-native config
	// schema takes byte sizes as plain integers (see
	// api/unbounded-storage/config.proto), so this is 2 GiB.
	storageDiskSize = 2 * 1024 * 1024 * 1024
)

// storageBinary returns the path to the built unbounded-storage binary.
func storageBinary() string {
	return filepath.Join(repoRoot(), "bin", "unbounded-storage")
}

// repoRoot returns the repository root by walking up from the current
// working directory until it finds the go.mod. This works whether the
// test runs via `go test ./internal/orca/inttest/...` (cwd is the
// package dir) or as a precompiled `go test -c` binary run from the
// repo root (cwd is the root), which is how `make orca-inttest-storage`
// invokes it under sudo.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}

	dir := filepath.Clean(wd)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return wd
		}

		dir = parent
	}
}

// storageRing is a running two-node unbounded-storage ring.
type storageRing struct {
	// FrontendAddrs holds the two frontend listen addresses
	// ("127.0.0.1:port"). Index 0 and 1 are distinct nodes; one owns
	// any given object's stripes, so fetching through both guarantees
	// the cross-node fabric RPC path is exercised.
	FrontendAddrs []string
}

// raiseMemlock raises RLIMIT_MEMLOCK to infinity so spawned storage
// children (which inherit our limits) can pin their io_uring buffers.
// Requires CAP_SYS_RESOURCE (root); the boundary test runs under sudo.
func raiseMemlock(t *testing.T) {
	t.Helper()

	lim := unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &lim); err != nil {
		t.Fatalf("raise RLIMIT_MEMLOCK (run under sudo): %v", err)
	}
}

// freeLoopbackPort returns a currently-free TCP port on loopback. There
// is an inherent race between closing the probe socket and the child
// binding it; this matches hack/smoke-storage.py and is acceptable for
// a serialized single-test ring.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc loopback port: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port //nolint:errcheck // *net.TCPAddr from net.Listen
	_ = ln.Close()                        //nolint:errcheck // probe socket close best-effort

	return port
}

// writeStorageConfig writes a single unbounded-storage node TOML whose
// S3 backend points at orcaEdge ("host:port"). The shape mirrors the
// config produced by hack/smoke-storage.py write_config: the schema is
// proto3-native, so byte sizes are plain integer byte counts and
// backend/frontend/peer/disk implementations are selected by oneof
// config table names. The startup-fixed knobs (heap backing, fabric
// bind address, forcing the tcp provider) live in the `[startup.*]` sections.
func writeStorageConfig(t *testing.T, path, fabricAddr string, localID, peerID int, peerAddr, diskPath, orcaEdge, frontendBind string) {
	t.Helper()

	cfg := fmt.Sprintf(`[[backends]]
name = "origin"

[backends.config.s3]
url = "%s"
stripe_size_bytes = %d

[[neighborhoods]]
name = "p2p"
source = "origin"
local_node_id = %d

[[neighborhoods.peers]]
id = %d

[neighborhoods.peers.config.tcp]
addr = "%s"

[[caches]]
name = "cache"
source = "p2p"

[[caches.disks]]
page_size_bytes = %d
skip_recovery_scan = true

[caches.disks.config.file]
path = "%s"
size = %d

[[frontends]]
name = "fe"
source = "cache"

[frontends.config.s3]
addr = "%s"

[startup.memory]
no_hugepages = true

[startup.fabric.binds.tcp]
addr = "%s"

[startup.topology]
disable_rdma = true
serving_cores = 2
`, orcaEdge, storageStripeSize,
		localID,
		peerID, peerAddr,
		storagePageSize, diskPath, storageDiskSize,
		frontendBind,
		fabricAddr)

	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write storage config %s: %v", path, err)
	}
}

// startStorageRing brings up a fresh two-node unbounded-storage ring
// whose S3 backends point at orcaEdge. It blocks until both frontend
// ports accept connections and registers process-group cleanup with
// t.Cleanup. The returned ring exposes the two frontend addresses.
func startStorageRing(ctx context.Context, t *testing.T, orcaEdge string) *storageRing {
	t.Helper()

	bin := storageBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("unbounded-storage binary not found at %s (run `make unbounded-storage-build`): %v", bin, err)
	}

	raiseMemlock(t)

	dir := t.TempDir()

	fabA, fabB := freeLoopbackPort(t), freeLoopbackPort(t)
	feA, feB := freeLoopbackPort(t), freeLoopbackPort(t)

	cfg1 := filepath.Join(dir, "node1.toml")
	cfg2 := filepath.Join(dir, "node2.toml")

	writeStorageConfig(t, cfg1,
		fmt.Sprintf("127.0.0.1:%d", fabA), 1, 2, fmt.Sprintf("127.0.0.1:%d", fabB),
		filepath.Join(dir, "node1.disk"), orcaEdge, fmt.Sprintf("127.0.0.1:%d", feA))
	writeStorageConfig(t, cfg2,
		fmt.Sprintf("127.0.0.1:%d", fabB), 2, 1, fmt.Sprintf("127.0.0.1:%d", fabA),
		filepath.Join(dir, "node2.disk"), orcaEdge, fmt.Sprintf("127.0.0.1:%d", feB))

	spawnStorageNode(ctx, t, bin, cfg1, filepath.Join(dir, "node1.log"))
	spawnStorageNode(ctx, t, bin, cfg2, filepath.Join(dir, "node2.log"))

	frontends := []string{
		fmt.Sprintf("127.0.0.1:%d", feA),
		fmt.Sprintf("127.0.0.1:%d", feB),
	}

	for _, addr := range frontends {
		waitForTCP(ctx, t, addr, 60*time.Second)
	}

	// Give the fabric peers a moment to dial each other before routing,
	// matching the smoke test's settle pause.
	time.Sleep(3 * time.Second)

	return &storageRing{FrontendAddrs: frontends}
}

// spawnStorageNode starts one unbounded-storage process in its own
// process group, redirecting combined output to logPath, and registers
// a t.Cleanup that kills the whole group.
func spawnStorageNode(ctx context.Context, t *testing.T, bin, cfgPath, logPath string) {
	t.Helper()

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log %s: %v", logPath, err)
	}

	// The only CLI argument is the config path; every startup-fixed knob
	// (heap backing via no_hugepages, fabric listen address, forcing the
	// tcp provider) now lives in the config file's `[startup.*]` sections.
	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath)
	cmd.Env = os.Environ() // inherit LD_LIBRARY_PATH for libfabric
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close() //nolint:errcheck // best-effort

		t.Fatalf("start unbounded-storage (%s): %v", cfgPath, err)
	}

	pid := cmd.Process.Pid

	t.Cleanup(func() {
		// Kill the entire process group (negative pid), escalating is
		// unnecessary: SIGKILL is immediate.
		_ = unix.Kill(-pid, unix.SIGKILL) //nolint:errcheck // best-effort teardown
		_ = cmd.Wait()                    //nolint:errcheck // reaps the child
		_ = logFile.Close()               //nolint:errcheck // best-effort

		if t.Failed() {
			dumpStorageLog(t, logPath)
		}
	})
}

// dumpStorageLog prints a node log to the test output on failure.
func dumpStorageLog(t *testing.T, logPath string) {
	t.Helper()

	b, err := os.ReadFile(logPath)
	if err != nil {
		return
	}

	t.Logf("--- %s ---\n%s", filepath.Base(logPath), string(b))
}

// waitForTCP blocks until a TCP connect to addr succeeds or the
// deadline elapses.
func waitForTCP(ctx context.Context, t *testing.T, addr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close() //nolint:errcheck // probe close best-effort
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", addr, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}

	t.Fatalf("timed out waiting for %s to accept connections", addr)
}

// fillDeterministic writes a deterministic, position-dependent byte
// pattern into buf for the object byte range starting at offset. The
// pattern matches deterministicBytes (out[i] = seed ^ byte(pos*31+17))
// so small (buffered) and large (streamed) blobs share one generator.
func fillDeterministic(buf []byte, offset int64, seed byte) {
	for i := range buf {
		pos := offset + int64(i)
		buf[i] = seed ^ byte(pos*31+17)
	}
}

// verifyDeterministicStream reads the entire body from r and asserts it
// equals `size` bytes of the deterministic pattern for seed, comparing
// in bounded-size windows so a 1 GiB body is never held in memory. It
// fails the test on the first mismatch or length disagreement.
func verifyDeterministicStream(t *testing.T, label string, r io.Reader, size int64, seed byte) {
	t.Helper()

	const window = 1 << 20 // 1 MiB compare window

	got := make([]byte, window)
	want := make([]byte, window)

	var pos int64

	for pos < size {
		n := int64(window)
		if remaining := size - pos; n > remaining {
			n = remaining
		}

		if _, err := io.ReadFull(r, got[:n]); err != nil {
			t.Fatalf("%s: read body at offset %d (want %d total): %v", label, pos, size, err)
		}

		fillDeterministic(want[:n], pos, seed)

		if !equalBytes(got[:n], want[:n]) {
			off := firstDiff(got[:n], want[:n])
			t.Fatalf("%s: body mismatch at absolute offset %d (got 0x%02x want 0x%02x)",
				label, pos+int64(off), got[off], want[off])
		}

		pos += n
	}

	// Confirm the body ends exactly at size (no trailing bytes).
	extra, err := r.Read(make([]byte, 1))
	if extra != 0 || (err != nil && err != io.EOF) {
		t.Fatalf("%s: body longer than expected %d bytes (extra=%d err=%v)", label, size, extra, err)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}

	return len(a)
}

// httpObjectURL formats a frontend address into a full URL for a key
// under the given bucket (path-style, forwarded verbatim to the orca
// edge by the unbounded-storage S3 backend).
func httpObjectURL(frontendAddr, bucket, key string) string {
	return "http://" + frontendAddr + "/" + bucket + "/" + key
}
