// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	racer "github.com/Azure/unbounded/pkg/racer"
)

func datasetForTest(t *testing.T, c config) *dataset {
	t.Helper()
	d := newDataset(t.Context(), c)
	t.Cleanup(d.Close)

	return d
}

func TestDatasetIdentityAndTargets(t *testing.T) {
	c := config{footprint: 3 * 257, objectSize: 257, ttl: 17 * time.Second}

	d := datasetForTest(t, c)
	if got := d.target(2); got != "/loadgen/v1/771/257/2" {
		t.Fatalf("target = %q", got)
	}

	ctx := context.Background()

	m, err := d.Stat(ctx, d.target(2))
	if err != nil || m.Size != 257 || m.ETag == "" || m.TTL == nil || *m.TTL != c.ttl {
		t.Fatalf("metadata = %+v, %v", m, err)
	}

	*m.TTL = 0

	again, err := datasetForTest(t, c).Stat(ctx, d.target(2))
	if err != nil || again.ETag != m.ETag || again.TTL == nil || *again.TTL != c.ttl {
		t.Fatalf("replica metadata = %+v, %v", again, err)
	}

	for _, id := range []int{0, 1} {
		other, err := d.Stat(ctx, d.target(id))
		if err != nil || other.ETag == m.ETag {
			t.Fatalf("object %d must have its own ETag: %+v, %v", id, other, err)
		}
	}

	for _, target := range []string{
		d.target(-1), d.target(3), d.prefix, d.prefix + "00", d.prefix + "+1", d.prefix + "1/",
		d.prefix + "1?", d.prefix + "1?x=y", d.prefix + "%31", d.prefix + "../1", d.prefix + "9223372036854775808",
		"/loadgen/v1/772/257/1", "/loadgen/v1/771/256/1",
	} {
		if _, err := d.Stat(ctx, target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q) = %v, want not-exist", target, err)
		}

		if _, err := d.Open(ctx, target, m.ETag); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want not-exist", target, err)
		}
	}

	for _, tag := range []string{"", "wrong", "W/" + m.ETag} {
		if _, err := d.Open(ctx, d.target(2), tag); !errors.Is(err, racer.ErrVersionChanged) {
			t.Errorf("Open with ETag %q = %v", tag, err)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := d.Stat(canceled, d.target(2)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Stat = %v", err)
	}

	if _, err := d.Open(canceled, d.target(2), m.ETag); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Open = %v", err)
	}
}

// Pause after a real chunk has been hashed, independent of CPU speed.
type pausedChecksumSource struct {
	io.ReaderAt
	ctx     context.Context
	reads   int
	release <-chan struct{}
}

func (s *pausedChecksumSource) ReadAt(p []byte, off int64) (int, error) {
	s.reads++

	if off > 0 {
		select {
		case <-s.release:
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		}
	}

	return s.ReaderAt.ReadAt(p, off)
}

func TestDatasetChecksumWaiters(t *testing.T) {
	for _, cancelOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_owner=%t", cancelOwner), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDataset(context.Background(), config{footprint: 65536, objectSize: 65536})
				defer d.Close()

				target := d.target(0)

				release := make(chan struct{})
				defer close(release)

				source := &pausedChecksumSource{ReaderAt: d.source(target), ctx: d.ctx, release: release}
				calls := 0
				d.hash = func(ctx context.Context, _ string) ([32]byte, error) {
					calls++
					return checksum(ctx, source, d.size)
				}

				ownerCtx, stopOwner := context.WithCancel(context.Background())
				defer stopOwner()

				ownerDone := make(chan error, 1)

				go func() {
					_, err := d.Stat(ownerCtx, target)
					ownerDone <- err
				}()

				synctest.Wait()

				canceledCtx, cancel := context.WithCancel(context.Background())
				defer cancel()

				deadlineCtx, stopDeadline := context.WithTimeout(context.Background(), time.Second)
				defer stopDeadline()

				canceledDone, deadlineDone := make(chan error, 1), make(chan error, 1)
				for ctx, done := range map[context.Context]chan error{canceledCtx: canceledDone, deadlineCtx: deadlineDone} {
					go func() {
						_, err := d.Stat(ctx, target)
						done <- err
					}()
				}

				liveDone := make(chan racer.Metadata, 1)

				go func() {
					m, err := d.Stat(context.Background(), target)
					if err != nil {
						t.Errorf("live waiter: %v", err)
					}

					liveDone <- m
				}()

				synctest.Wait()
				cancel()
				time.Sleep(time.Second)
				synctest.Wait()

				for done, want := range map[chan error]error{canceledDone: context.Canceled, deadlineDone: context.DeadlineExceeded} {
					select {
					case err := <-done:
						if !errors.Is(err, want) {
							t.Errorf("waiter error = %v, want %v", err, want)
						}
					default:
						t.Fatal("canceled waiter did not return while checksum owner was paused")
					}
				}

				select {
				case <-liveDone:
					t.Fatal("live waiter returned before checksum completion")
				default:
				}

				var wantOwnerErr error

				if cancelOwner {
					stopOwner()

					wantOwnerErr = context.Canceled

					synctest.Wait()

					select {
					case err := <-ownerDone:
						if !errors.Is(err, wantOwnerErr) {
							t.Fatalf("owner error = %v", err)
						}
					default:
						t.Fatal("canceled requester did not return while publication was paused")
					}
				}

				release <- struct{}{}

				if !cancelOwner {
					if err := <-ownerDone; err != nil {
						t.Errorf("owner error = %v", err)
					}
				}

				payload := make([]byte, d.size)
				if _, err := d.source(target).ReadAt(payload, 0); err != nil {
					t.Fatal(err)
				}

				wantETag := fmt.Sprintf(`"%x"`, sha256.Sum256(payload))
				if m := <-liveDone; m.Size != d.size || m.ETag != wantETag {
					t.Fatalf("live waiter metadata = %+v, want ETag %s", m, wantETag)
				}

				if _, err := d.Stat(context.Background(), target); err != nil {
					t.Fatal(err)
				}

				if calls != 1 || source.reads != 2 {
					t.Fatalf("hash calls/reads = %d/%d, want 1/2 without restarting progress", calls, source.reads)
				}
			})
		})
	}
}

func TestDatasetChecksumOutlivesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newDataset(context.Background(), config{footprint: 65536, objectSize: 65536})
		defer d.Close()

		release := make(chan struct{})
		defer close(release)

		target := d.target(0)
		source := &pausedChecksumSource{ReaderAt: d.source(target), ctx: d.ctx, release: release}
		calls := 0
		d.hash = func(ctx context.Context, _ string) ([32]byte, error) {
			calls++
			return checksum(ctx, source, d.size)
		}

		// Model repeated cold HEAD deadlines while the same partially hashed
		// object remains in publication, including periods with no waiters.
		for range 3 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			done := make(chan error, 1)

			go func() {
				_, err := d.Stat(ctx, target)
				done <- err
			}()

			synctest.Wait()
			time.Sleep(30 * time.Second)

			if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cold HEAD = %v", err)
			}

			cancel()
		}

		release <- struct{}{}

		synctest.Wait()

		if !d.checksums[0].ready || calls != 1 || source.reads != 2 {
			t.Fatalf("publication without waiters: ready=%t calls=%d reads=%d", d.checksums[0].ready, calls, source.reads)
		}

		payload := make([]byte, d.size)
		if _, err := d.source(target).ReadAt(payload, 0); err != nil {
			t.Fatal(err)
		}

		m, err := d.Stat(context.Background(), target)
		if err != nil || m.ETag != fmt.Sprintf(`"%x"`, sha256.Sum256(payload)) {
			t.Fatalf("warm HEAD = %+v, %v", m, err)
		}
	})
}

func TestDatasetPublicationBoundAndShutdown(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent_cancel=%t", parentCancel), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				d := newDataset(ctx, config{footprint: 16, objectSize: 1})
				defer d.Close()

				release := make(chan struct{})
				defer close(release)

				calls := 0
				exited := false
				d.hash = func(ctx context.Context, _ string) ([32]byte, error) {
					calls++

					<-ctx.Done()
					<-release

					exited = true

					return [32]byte{}, ctx.Err()
				}

				results := make(chan error, 48)

				for range 3 {
					for id := range int(d.count) {
						go func() {
							_, err := d.Stat(context.Background(), d.target(id))
							results <- err
						}()
					}
				}

				synctest.Wait()

				if calls != 1 || len(d.jobs) != int(d.count)-1 {
					t.Fatalf("hash jobs=%d queued=%d; want 1 active and one queued per remaining object", calls, len(d.jobs))
				}

				if parentCancel {
					cancel()
				}

				closed := make(chan struct{})

				go func() {
					d.Close()
					close(closed)
				}()

				synctest.Wait()

				select {
				case <-closed:
					t.Fatal("Close returned before publication exited")
				default:
				}

				for range 48 {
					if err := <-results; !errors.Is(err, context.Canceled) {
						t.Fatalf("shutdown waiter = %v", err)
					}
				}

				release <- struct{}{}

				<-closed

				if !exited || calls != 1 {
					t.Fatalf("shutdown exited=%t calls=%d; queued jobs must not start", exited, calls)
				}

				for id := range int(d.count) {
					if d.checksums[id].ready {
						t.Fatal("shutdown published an incomplete checksum")
					}

					if _, err := d.Stat(context.Background(), d.target(id)); !errors.Is(err, context.Canceled) {
						t.Fatalf("Stat after Close = %v", err)
					}
				}
			})
		})
	}
}

func TestDatasetPublicationFailure(t *testing.T) {
	d := datasetForTest(t, config{footprint: 1, objectSize: 1})
	want := errors.New("checksum source failed")
	calls := 0

	d.hash = func(context.Context, string) ([32]byte, error) {
		calls++
		return [32]byte{}, want
	}
	for range 2 {
		if _, err := d.Stat(context.Background(), d.target(0)); !errors.Is(err, want) {
			t.Fatalf("publication error = %v", err)
		}
	}

	if calls != 1 || d.checksums[0].ready {
		t.Fatalf("failed publication calls=%d ready=%t", calls, d.checksums[0].ready)
	}
}

func sourceForTest(t *testing.T, d *dataset, id int) racer.Source {
	t.Helper()

	m, err := d.Stat(context.Background(), d.target(id))
	if err != nil {
		t.Fatal(err)
	}

	s, err := d.Open(context.Background(), d.target(id), m.ETag)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	return s
}

func TestSyntheticReadAtAlignmentSplitsAndEOF(t *testing.T) {
	c := config{footprint: 3 * 4099, objectSize: 4099}
	d := datasetForTest(t, c)
	s := sourceForTest(t, d, 1)

	whole := make([]byte, d.size)
	if n, err := s.ReadAt(whole, 0); n != len(whole) || err != nil {
		t.Fatalf("full read = %d, %v", n, err)
	}

	meta, err := d.Stat(context.Background(), d.target(1))
	if err != nil || meta.ETag != fmt.Sprintf(`"%x"`, sha256.Sum256(whole)) {
		t.Fatalf("ETag must be the exact content checksum: %+v, %v", meta, err)
	}
	// Every byte alignment and a range of short/long reads cross word boundaries.
	for off := 0; off < 32; off++ {
		for _, length := range []int{1, 7, 8, 9, 31, 256, 1025} {
			got := make([]byte, length)
			if n, err := s.ReadAt(got, int64(off)); n != length || err != nil || !bytes.Equal(got, whole[off:off+length]) {
				t.Fatalf("ReadAt(off=%d, len=%d) = %d, %v; bytes agree: %v", off, length, n, err, bytes.Equal(got, whole[off:off+length]))
			}
		}
	}
	// A separate replica, read in irregular pieces, must reproduce the same object.
	replica := sourceForTest(t, datasetForTest(t, c), 1)
	split := make([]byte, len(whole))

	rng := rand.New(rand.NewSource(29))
	for off := 0; off < len(split); {
		end := min(len(split), off+1+rng.Intn(117))
		if n, err := replica.ReadAt(split[off:end], int64(off)); n != end-off || err != nil {
			t.Fatalf("split read = %d, %v", n, err)
		}

		off = end
	}

	if !bytes.Equal(split, whole) || bytes.Equal(whole, make([]byte, len(whole))) || bytes.Equal(whole[:64], whole[64:128]) {
		t.Fatal("synthetic data is inconsistent or repeats a constant block")
	}

	other := make([]byte, len(whole))
	if _, err := sourceForTest(t, d, 2).ReadAt(other, 0); err != nil || bytes.Equal(other, whole) {
		t.Fatalf("distinct objects must have distinct payloads: %v", err)
	}

	for _, tc := range []struct {
		name          string
		off           int64
		length, wantN int
		wantErr       error
	}{
		{"exact end", d.size - 7, 7, 7, nil},
		{"cross end", d.size - 7, 19, 7, io.EOF},
		{"at end", d.size, 19, 0, io.EOF},
		{"past end", math.MaxInt64, 19, 0, io.EOF},
		{"empty", 0, 0, 0, nil},
		{"empty at end", d.size, 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := bytes.Repeat([]byte{0xa5}, tc.length)

			n, err := s.ReadAt(buf, tc.off)
			if n != tc.wantN || !errors.Is(err, tc.wantErr) {
				t.Fatalf("read = %d, %v; want %d, %v", n, err, tc.wantN, tc.wantErr)
			}

			if n > 0 && !bytes.Equal(buf[:n], whole[tc.off:tc.off+int64(n)]) {
				t.Error("tail payload differs")
			}

			if !bytes.Equal(buf[n:], bytes.Repeat([]byte{0xa5}, tc.length-n)) {
				t.Error("read modified bytes beyond its returned count")
			}
		})
	}

	if n, err := s.ReadAt(make([]byte, 8), -1); n != 0 || err == nil {
		t.Fatalf("negative offset = %d, %v", n, err)
	}
}

func TestHandlerPreservesRawTargets(t *testing.T) {
	d := datasetForTest(t, config{footprint: 128, objectSize: 64})
	reg := prometheus.NewRegistry()
	newMetrics(reg)

	h, _ := racer.NewOrigin(d)
	for target, want := range map[string]int{
		"/healthz": 404, "/metrics": 404, d.target(0): 200,
		d.prefix + "00": 404, d.prefix + "%30": 404, d.prefix + "./0": 404,
		d.prefix + "../64/0": 404, d.prefix + "/0": 404, d.target(0) + "?": 404,
		"/healthz?": 404, "/metrics?": 404,
	} {
		t.Run(target, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

			if w.Code != want || w.Header().Get("Location") != "" {
				t.Fatalf("status = %d, Location = %q; want %d without redirect", w.Code, w.Header().Get("Location"), want)
			}
		})
	}
}
