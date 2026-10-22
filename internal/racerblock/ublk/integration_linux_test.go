// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Privileged tests. Run with:
//
//	sudo modprobe ublk_drv
//	go test -c -o /tmp/opencode/ublk.test ./internal/racerblock/ublk
//	sudo /tmp/opencode/ublk.test -test.v -test.run Integration

const testDevSize = 64<<20 + 512

func requireUblk(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	if _, err := os.Stat(controlPath); err != nil {
		t.Skipf("%s unavailable: %v", controlPath, err)
	}
}

// patternByte is the deterministic device content at off.
func patternByte(off int64) byte {
	w := uint64(off>>3) * 0x9E3779B97F4A7C15
	return byte(w >> (8 * (off & 7)))
}

func fillPattern(p []byte, off int64) {
	for i := range p {
		p[i] = patternByte(off + int64(i))
	}
}

type patternHandler struct {
	size   int64
	failAt atomic.Int64 // offset range start that fails with EIO; -1 none
	calls  atomic.Int64
}

func newPatternHandler(size int64) *patternHandler {
	h := &patternHandler{size: size}
	h.failAt.Store(-1)

	return h
}

func (h *patternHandler) ReadAt(_ context.Context, p []byte, off int64) error {
	h.calls.Add(1)

	if off < 0 || off+int64(len(p)) > h.size {
		return syscall.EINVAL
	}

	if f := h.failAt.Load(); f >= 0 && off <= f && f < off+int64(len(p)) {
		return errors.New("injected failure")
	}

	fillPattern(p, off)

	return nil
}

func startTestDevice(t *testing.T, ctx context.Context, h Handler, cfg Config) *Device {
	t.Helper()

	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	d, err := Start(ctx, cfg, h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	t.Cleanup(func() { _ = d.Close() })

	return d
}

// alignedBuf returns a 4096-aligned buffer of n bytes for O_DIRECT.
func alignedBuf(n int) []byte {
	raw := make([]byte, n+4096)
	off := (4096 - int(uintptr(unsafe.Pointer(&raw[0]))&4095)) & 4095

	return raw[off : off+n]
}

func openDirect(t *testing.T, path string) int {
	t.Helper()

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	t.Cleanup(func() { _ = unix.Close(fd) })

	return fd
}

func checkRead(t *testing.T, fd int, off int64, n int) {
	t.Helper()

	buf := alignedBuf(n)

	got, err := unix.Pread(fd, buf, off)
	if err != nil {
		t.Fatalf("pread off=%d len=%d: %v", off, n, err)
	}

	want := make([]byte, got)
	fillPattern(want, off)

	if !bytes.Equal(buf[:got], want) {
		t.Fatalf("data mismatch off=%d len=%d", off, n)
	}

	if remain := int64(testDevSize) - off; int64(got) != min(int64(n), remain) {
		t.Fatalf("short read off=%d len=%d got=%d", off, n, got)
	}
}

func TestIntegrationReadOnlyDevice(t *testing.T) {
	requireUblk(t)

	h := newPatternHandler(testDevSize)
	cfg := Config{ID: -1, Size: testDevSize, Queues: 2, Depth: 64}
	d := startTestDevice(t, context.Background(), h, cfg)

	if !strings.HasPrefix(d.Path(), "/dev/ublkb") || d.Path() != "/dev/ublkb"+strconv.Itoa(d.ID()) {
		t.Fatalf("path %q id %d", d.Path(), d.ID())
	}

	if _, err := os.Stat(d.Path()); err != nil {
		t.Fatal(err)
	}

	t.Run("size", func(t *testing.T) {
		fd := openDirect(t, d.Path())

		var sz uint64
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&sz))); e != 0 {
			t.Fatal(e)
		}

		if sz != testDevSize {
			t.Fatalf("size %d", sz)
		}
	})

	t.Run("readonly", func(t *testing.T) {
		ro, err := os.ReadFile("/sys/block/ublkb" + strconv.Itoa(d.ID()) + "/ro")
		if err != nil {
			t.Fatal(err)
		}

		if strings.TrimSpace(string(ro)) != "1" {
			t.Fatalf("ro = %q", ro)
		}

		fd, err := unix.Open(d.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
		if err == nil {
			defer unix.Close(fd)

			if _, err := unix.Pwrite(fd, alignedBuf(4096), 0); err == nil {
				t.Fatal("write to read-only device succeeded")
			}
		}
	})

	t.Run("random reads", func(t *testing.T) {
		fd := openDirect(t, d.Path())
		rng := rand.New(rand.NewPCG(1, 2))
		sizes := []int{512, 4096, 8192, 65536, 128 << 10, d.cfg.MaxIOBytes, 2 * d.cfg.MaxIOBytes}

		// Fixed edge cases: start, end, crossing max-io boundaries and the tail.
		checkRead(t, fd, 0, d.cfg.MaxIOBytes)
		checkRead(t, fd, int64(d.cfg.MaxIOBytes)-4096, 8192)
		checkRead(t, fd, testDevSize-512, 512)
		checkRead(t, fd, testDevSize-4096-512, 8192) // runs past the end: short read
		checkRead(t, fd, 32<<20-512, 4096)

		for range 300 {
			n := sizes[rng.IntN(len(sizes))]
			off := rng.Int64N(testDevSize/512) * 512
			checkRead(t, fd, off, n)
		}
	})

	t.Run("concurrent readers", func(t *testing.T) {
		var wg sync.WaitGroup

		errc := make(chan error, 16)

		for w := range 16 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				fd, err := unix.Open(d.Path(), unix.O_RDONLY|unix.O_DIRECT, 0)
				if err != nil {
					errc <- err
					return
				}
				defer unix.Close(fd)

				rng := rand.New(rand.NewPCG(uint64(w), 9))
				buf := alignedBuf(256 << 10)

				for range 100 {
					n := (1 + rng.IntN(64)) * 4096
					off := rng.Int64N((testDevSize-int64(n))/4096) * 4096

					if _, err := unix.Pread(fd, buf[:n], off); err != nil {
						errc <- err
						return
					}

					want := make([]byte, n)
					fillPattern(want, off)

					if !bytes.Equal(buf[:n], want) {
						errc <- errors.New("mismatch at " + strconv.FormatInt(off, 10))
						return
					}
				}
			}()
		}

		wg.Wait()
		close(errc)

		for err := range errc {
			t.Error(err)
		}
	})

	t.Run("handler error is EIO", func(t *testing.T) {
		fd := openDirect(t, d.Path())

		h.failAt.Store(40 << 20)
		defer h.failAt.Store(-1)

		_, err := unix.Pread(fd, alignedBuf(4096), 40<<20)
		if !errors.Is(err, unix.EIO) {
			t.Fatalf("got %v, want EIO", err)
		}

		checkRead(t, fd, 0, 4096) // device still healthy
	})

	t.Run("close", func(t *testing.T) {
		path := d.Path()

		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				if err := d.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
		}

		wg.Wait()

		select {
		case <-d.Done():
		default:
			t.Fatal("Done not closed")
		}

		if d.Err() != nil {
			t.Fatalf("Err after clean close: %v", d.Err())
		}

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", path, err)
		}

		if err := Delete(d.ID()); err == nil {
			t.Fatal("Delete of closed device succeeded")
		}
	})
}

// blockingHandler blocks until ctx is canceled, to check that teardown
// cancels in-flight handlers.
type blockingHandler struct{ entered chan struct{} }

func (b *blockingHandler) ReadAt(ctx context.Context, _ []byte, _ int64) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}

	<-ctx.Done()

	return ctx.Err()
}

func TestIntegrationContextCancel(t *testing.T) {
	requireUblk(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bh := &blockingHandler{entered: make(chan struct{}, 1)}
	d := startTestDevice(t, ctx, bh, Config{ID: -1, Size: 1 << 20, Queues: 1, Depth: 4})
	path := d.Path()

	readErr := make(chan error, 1)

	go func() {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECT, 0)
		if err != nil {
			readErr <- err
			return
		}
		defer unix.Close(fd)

		_, err = unix.Pread(fd, alignedBuf(4096), 4096)
		readErr <- err
	}()

	select {
	case <-bh.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never called")
	}

	cancel()

	select {
	case <-d.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("device did not stop after ctx cancel")
	}

	if !errors.Is(d.Err(), context.Canceled) {
		t.Fatalf("Err = %v", d.Err())
	}

	if err := <-readErr; err == nil {
		t.Fatal("blocked read succeeded")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists: %v", path, err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close after cancel: %v", err)
	}
}

func TestIntegrationFixedIDAndDelete(t *testing.T) {
	requireUblk(t)

	// Find a free id.
	id := 900
	for ; id < 1000; id++ {
		if _, err := os.Stat("/dev/ublkc" + strconv.Itoa(id)); os.IsNotExist(err) {
			break
		}
	}

	d := startTestDevice(t, context.Background(), newPatternHandler(1<<20), Config{ID: id, Size: 1 << 20})
	if d.ID() != id {
		t.Fatalf("id %d, want %d", d.ID(), id)
	}

	// A second device with the same id must fail and clean up after itself.
	if _, err := Start(context.Background(), Config{ID: id, Size: 1 << 20}, newPatternHandler(1<<20)); err == nil {
		t.Fatal("duplicate id accepted")
	}

	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Delete(id); err == nil {
		t.Fatal("Delete of removed device succeeded")
	}

	if err := Delete(4000); err == nil {
		t.Fatal("Delete of nonexistent device succeeded")
	}

	if err := Delete(-1); err == nil {
		t.Fatal("Delete(-1) succeeded")
	}
}

func TestIntegrationStartValidation(t *testing.T) {
	if _, err := Start(context.Background(), Config{ID: -1, Size: 1000}, newPatternHandler(1000)); err == nil {
		t.Fatal("bad size accepted")
	}

	if _, err := Start(context.Background(), Config{ID: -1, Size: 4096}, nil); err == nil {
		t.Fatal("nil handler accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Start(ctx, Config{ID: -1, Size: 4096}, newPatternHandler(4096)); err == nil {
		t.Fatal("canceled ctx accepted")
	}
}

// TestIntegrationDeleteIDs is a manual cleanup helper: it deletes the
// comma-separated device ids in UBLK_TEST_DELETE_IDS, for example devices
// left behind by a killed test binary.
func TestIntegrationDeleteIDs(t *testing.T) {
	requireUblk(t)

	ids := os.Getenv("UBLK_TEST_DELETE_IDS")
	if ids == "" {
		t.Skip("UBLK_TEST_DELETE_IDS not set")
	}

	for s := range strings.SplitSeq(ids, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatal(err)
		}

		if err := Delete(id); err != nil {
			t.Errorf("Delete(%d): %v", id, err)
		}

		if _, err := os.Stat("/dev/ublkc" + strconv.Itoa(id)); !os.IsNotExist(err) {
			t.Errorf("/dev/ublkc%d still present: %v", id, err)
		}
	}
}

// TestIntegrationDeleteStale starts a device in a child process, kills the
// child with SIGKILL and checks that Delete removes the leftover device.
func TestIntegrationDeleteStale(t *testing.T) {
	requireUblk(t)

	if os.Getenv("UBLK_TEST_CHILD") == "1" {
		d, err := Start(context.Background(), Config{ID: -1, Size: 1 << 20, Queues: 1}, newPatternHandler(1<<20))
		if err != nil {
			os.Stdout.WriteString("ERR " + err.Error() + "\n")
			os.Exit(1)
		}

		os.Stdout.WriteString("ID " + strconv.Itoa(d.ID()) + "\n")

		select {}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationDeleteStale$")

	cmd.Env = append(os.Environ(), "UBLK_TEST_CHILD=1")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(out).ReadString('\n')
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	if err != nil || !strings.HasPrefix(line, "ID ") {
		t.Fatalf("child: %q %v", line, err)
	}

	id, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "ID ")))
	if err != nil {
		t.Fatal(err)
	}

	if err := Delete(id); err != nil {
		t.Fatalf("Delete(%d): %v", id, err)
	}

	for _, p := range []string{"/dev/ublkc", "/dev/ublkb"} {
		if _, err := os.Stat(p + strconv.Itoa(id)); !os.IsNotExist(err) {
			t.Errorf("%s%d still present: %v", p, id, err)
		}
	}
}
