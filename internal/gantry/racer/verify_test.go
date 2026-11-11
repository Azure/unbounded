// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type recordingInvalidator struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingInvalidator) Invalidate(ctx context.Context, key racersdk.Key, etag string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, key.String()+" "+etag)

	return r.err
}

func (r *recordingInvalidator) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.calls...)
}

// verifyServer serves ref (with optional offset) from a fake Racer whose
// origin returns served for an object declared to have digest want.
func verifyServer(t *testing.T, want, served []byte, offset int64, opts ...HandlerOption) (*httptest.Server, ifaces.OriginRef) {
	t.Helper()

	ref := ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: racerDigest(want), Kind: ifaces.KindBlob}
	client := racerFakeClient(t, racerPageOrigin(t, ref.Digest, served))
	h := NewHandler(client, nil, nil, opts...)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served := ref
		served.Offset = offset
		h.ServeContent(w, r, served)
	}))
	t.Cleanup(server.Close)

	return server, ref
}

func verifyGet(t *testing.T, server *httptest.Server) (*http.Response, []byte, error) {
	t.Helper()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	body, err := io.ReadAll(resp.Body)

	return resp, body, err
}

func TestVerifiedFullResponse(t *testing.T) {
	for _, data := range [][]byte{[]byte("x"), bytes.Repeat([]byte("0123456789abcdef"), 64*1024)} {
		inv := &recordingInvalidator{}

		var mismatches atomic.Int32

		server, _ := verifyServer(t, data, data, 0, WithInvalidator(inv), WithDigestMismatchObserver(func() { mismatches.Add(1) }))

		resp, body, err := verifyGet(t, server)
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("status=%d bytes=%d err=%v", resp.StatusCode, len(body), err)
		}

		if len(inv.recorded()) != 0 || mismatches.Load() != 0 {
			t.Fatal("verified response invalidated")
		}
	}
}

func TestCorruptFullResponseAbortsAndInvalidates(t *testing.T) {
	for name, size := range map[string]int{"small": 5, "multi chunk": 1 << 20} {
		t.Run(name, func(t *testing.T) {
			want := bytes.Repeat([]byte("a"), size)
			corrupt := bytes.Clone(want)
			corrupt[size/2] = 'b'

			inv := &recordingInvalidator{err: errors.New("best effort")}

			var mismatches atomic.Int32

			server, ref := verifyServer(t, want, corrupt, 0, WithInvalidator(inv), WithDigestMismatchObserver(func() { mismatches.Add(1) }))

			resp, body, err := verifyGet(t, server)
			if resp.StatusCode != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("status=%d err=%v; want aborted 200", resp.StatusCode, err)
			}

			// Buffered bytes may be dropped by the abort; the final byte never leaves.
			if len(body) >= size || !bytes.Equal(body, corrupt[:len(body)]) {
				t.Fatalf("got %d of %d bytes; want a strict prefix", len(body), size)
			}

			wantCall := ref.Digest.Hex() + ` "` + ref.Digest.String() + `"`
			if calls := inv.recorded(); len(calls) != 1 || calls[0] != wantCall {
				t.Fatalf("invalidate calls = %q; want %q", calls, wantCall)
			}

			if mismatches.Load() != 1 {
				t.Fatalf("mismatches = %d; want 1", mismatches.Load())
			}
		})
	}
}

func TestCorruptFullResponseForgetsLocalReplica(t *testing.T) {
	want := []byte("expected")
	replicas := NewReplicaVerifier(nil, nil)
	server, ref := verifyServer(t, want, []byte("tampered"), 0, WithLocalReplicaReset(replicas))

	id := replicaID{digest: ref.Digest.String(), size: int64(len(want))}
	replicas.entries[id] = &replicaEntry{state: replicaGood, done: make(chan struct{})}

	if _, _, err := verifyGet(t, server); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v; want unexpected EOF", err)
	}

	replicas.mu.Lock()
	defer replicas.mu.Unlock()

	if _, ok := replicas.entries[id]; ok {
		t.Fatal("verification result survived a digest mismatch")
	}
}

func TestCorruptFullResponseWithoutInvalidator(t *testing.T) {
	want := []byte("expected")
	server, _ := verifyServer(t, want, []byte("tampered"), 0)

	if _, _, err := verifyGet(t, server); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v; want unexpected EOF", err)
	}
}

func TestRangedResponseIsNotVerified(t *testing.T) {
	want := []byte("0123456789")
	corrupt := []byte("0123456780")
	inv := &recordingInvalidator{}
	server, _ := verifyServer(t, want, corrupt, 4, WithInvalidator(inv))

	resp, body, err := verifyGet(t, server)
	if err != nil || resp.StatusCode != http.StatusPartialContent || string(body) != "456780" {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
	}

	if len(inv.recorded()) != 0 {
		t.Fatal("ranged response invalidated")
	}
}

func TestVerifyingWriterEmptyObject(t *testing.T) {
	empty := racerDigest(nil)

	if v := newVerifyingWriter(httptest.NewRecorder(), empty, 0); !v.verify() {
		t.Fatal("empty object did not verify")
	}

	if v := newVerifyingWriter(httptest.NewRecorder(), racerDigest([]byte("x")), 0); v.verify() {
		t.Fatal("empty body verified against a nonempty digest")
	}
}

func TestVerifyingWriterRejectsExcess(t *testing.T) {
	v := newVerifyingWriter(httptest.NewRecorder(), racerDigest([]byte("ab")), 2)
	if _, err := v.Write([]byte("abc")); err == nil {
		t.Fatal("excess bytes accepted")
	}

	if n, err := v.Write([]byte("a")); n != 1 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}

	if n, err := v.Write([]byte("b")); n != 1 || err != nil || !v.verify() {
		t.Fatalf("final Write = %d, %v", n, err)
	}
}

func TestDigestMismatchMetric(t *testing.T) {
	m := NewMetrics(metrics.New())
	m.DigestMismatch()
	m.DigestMismatch()

	if got := testutil.ToFloat64(m.mismatches); got != 2 {
		t.Fatalf("mismatches = %v, want 2", got)
	}
}
