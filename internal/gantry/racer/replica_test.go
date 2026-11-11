// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/contentstore"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// gatedStore counts opens and can block them until released, so tests can
// tell hashes (which open their own handle) apart and hold one in flight.
type gatedStore struct {
	inner LocalStore
	opens atomic.Int64
	gate  chan struct{} // nil means never block
}

func (g *gatedStore) Open(d string) (LocalBlob, int64, error) {
	n := g.opens.Add(1)
	if g.gate != nil && n%2 == 0 { // even opens are verifier opens in these tests
		<-g.gate
	}

	return g.inner.Open(d)
}

// replicaFixture writes data under the digest of want (corrupt when they
// differ) and returns the store, ref, and blob path.
func replicaFixture(t *testing.T, data, want []byte) (LocalStore, ifaces.OriginRef, string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "blobs", "sha256")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ref := testRef()
	ref.Digest = digestOf(want)
	path := filepath.Join(dir, ref.Digest.Hex())

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := contentstore.New(root)
	if err != nil {
		t.Fatal(err)
	}

	return ContentStore(store), ref, path
}

type resultLog struct {
	mu      sync.Mutex
	results []string
}

func (r *resultLog) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.results = append(r.results, s)
}

func (r *resultLog) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.results...)
}

func localRead(ctx context.Context, store LocalStore, v *ReplicaVerifier, ref ifaces.OriginRef) (string, error) {
	_, body, _, err := serveLocal(ctx, store, v, ref, racersdk.OriginRequest{
		Length: racersdk.PageSize, LocalOnly: true,
	}, true)
	if err != nil {
		return "", err
	}

	defer body.Close()

	data, err := io.ReadAll(body)

	return string(data), err
}

func TestReplicaVerifierServesGoodFileAfterOneHash(t *testing.T) {
	data := []byte("healthy blob bytes")
	inner, ref, _ := replicaFixture(t, data, data)
	store := &gatedStore{inner: inner}
	log := &resultLog{}
	v := NewReplicaVerifier(nil, log.add)

	for range 3 {
		got, err := localRead(t.Context(), store, v, ref)
		if err != nil || got != string(data) {
			t.Fatalf("read = %q, %v", got, err)
		}
	}

	// Three request opens plus one verifier open.
	if n := store.opens.Load(); n != 4 {
		t.Fatalf("opens = %d, want 4", n)
	}

	if got := log.get(); len(got) != 1 || got[0] != ReplicaVerified {
		t.Fatalf("results = %v", got)
	}
}

func TestReplicaVerifierQuarantinesCorruptFile(t *testing.T) {
	want := []byte("the real content")
	inner, ref, _ := replicaFixture(t, []byte("the rotted bytes"), want)
	store := &gatedStore{inner: inner}
	log := &resultLog{}
	v := NewReplicaVerifier(nil, log.add)

	for range 3 {
		_, err := localRead(t.Context(), store, v, ref)
		if !errors.Is(err, racersdk.ErrNotFound) || !errors.Is(err, errReplicaCorrupt) {
			t.Fatalf("err = %v, want corrupt local miss", err)
		}
	}

	if n := store.opens.Load(); n != 4 {
		t.Fatalf("opens = %d, want 4 (hashed once)", n)
	}

	if got := log.get(); len(got) != 1 || got[0] != ReplicaCorrupt {
		t.Fatalf("results = %v", got)
	}
}

func TestReplicaVerifierRechecksReplacedFile(t *testing.T) {
	want := []byte("the real content")
	store, ref, path := replicaFixture(t, []byte("the rotted bytes"), want)
	v := NewReplicaVerifier(nil, nil)

	if _, err := localRead(t.Context(), store, v, ref); !errors.Is(err, errReplicaCorrupt) {
		t.Fatalf("err = %v, want corrupt", err)
	}

	// containerd replaces a blob by writing a new file and renaming it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	got, err := localRead(t.Context(), store, v, ref)
	if err != nil || got != string(want) {
		t.Fatalf("read after repair = %q, %v", got, err)
	}
}

func TestReplicaVerifierForgetRechecks(t *testing.T) {
	data := []byte("healthy blob bytes")
	inner, ref, _ := replicaFixture(t, data, data)
	store := &gatedStore{inner: inner}
	v := NewReplicaVerifier(nil, nil)

	if _, err := localRead(t.Context(), store, v, ref); err != nil {
		t.Fatal(err)
	}

	v.Forget(ref.Digest.String())

	if _, err := localRead(t.Context(), store, v, ref); err != nil {
		t.Fatal(err)
	}

	if n := store.opens.Load(); n != 4 {
		t.Fatalf("opens = %d, want 4 (hashed twice)", n)
	}

	var nilVerifier *ReplicaVerifier
	nilVerifier.Forget(ref.Digest.String()) // must not panic
}

func TestReplicaVerifierContextExpiryMissesThenHits(t *testing.T) {
	data := []byte("healthy blob bytes")
	inner, ref, _ := replicaFixture(t, data, data)
	store := &gatedStore{inner: inner, gate: make(chan struct{})}
	v := NewReplicaVerifier(nil, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	if _, err := localRead(ctx, store, v, ref); !errors.Is(err, racersdk.ErrNotFound) || !errors.Is(err, errReplicaUnverified) {
		t.Fatalf("err = %v, want unverified local miss", err)
	}

	close(store.gate) // let the background hash finish

	deadline := time.Now().Add(5 * time.Second)

	for {
		got, err := localRead(t.Context(), store, v, ref)
		if err == nil && got == string(data) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("read never succeeded: %v", err)
		}

		time.Sleep(5 * time.Millisecond)
	}

	if n := store.opens.Load(); n != 3 {
		t.Fatalf("opens = %d, want 3 (one background hash)", n)
	}
}

func TestReplicaVerifierSingleflight(t *testing.T) {
	data := []byte("healthy blob bytes")
	inner, ref, _ := replicaFixture(t, data, data)
	log := &resultLog{}
	v := NewReplicaVerifier(nil, log.add)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if got, err := localRead(t.Context(), inner, v, ref); err != nil || got != string(data) {
				t.Errorf("read = %q, %v", got, err)
			}
		})
	}

	wg.Wait()

	if got := log.get(); len(got) != 1 {
		t.Fatalf("results = %v, want one hash", got)
	}
}

func TestReplicaVerifierCacheBound(t *testing.T) {
	v := NewReplicaVerifier(nil, nil)
	v.maxCache = 3

	root := t.TempDir()
	dir := filepath.Join(root, "blobs", "sha256")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cs, err := contentstore.New(root)
	if err != nil {
		t.Fatal(err)
	}

	store := ContentStore(cs)

	for i := range 10 {
		data := []byte{byte(i), 'x'}
		ref := testRef()
		ref.Digest = digestOf(data)

		if err := os.WriteFile(filepath.Join(dir, ref.Digest.Hex()), data, 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := localRead(t.Context(), store, v, ref); err != nil {
			t.Fatal(err)
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.entries) != 3 || v.pending != 0 {
		t.Fatalf("entries = %d pending = %d, want 3 and 0", len(v.entries), v.pending)
	}
}

type plainBlob struct{ io.ReaderAt }

func (plainBlob) Close() error { return nil }

func TestReplicaVerifierInlineWithoutStat(t *testing.T) {
	v := NewReplicaVerifier(nil, nil)
	data := []byte("abc")
	d := digestOf(data).String()

	if err := v.check(t.Context(), nil, d, plainBlob{bytesReaderAt(data)}, 3); err != nil {
		t.Fatalf("good inline = %v", err)
	}

	if err := v.check(t.Context(), nil, d, plainBlob{bytesReaderAt([]byte("abd"))}, 3); !errors.Is(err, errReplicaCorrupt) {
		t.Fatalf("bad inline = %v", err)
	}
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}

	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}
