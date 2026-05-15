// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/cluster"
)

// e2e_test.go is the canonical end-to-end suite for orca: every
// scenario runs against a 3-replica in-process cluster pointed at
// LocalStack. Tests that exercise chunk fetching naturally exercise
// both the local-fill path (when self happens to win rendezvous for
// a chunk) and the cross-replica /internal/fill path (when a peer
// wins).
//
// Driver-level branch coverage (versioning gate, blob-type rejection,
// HTTP error mapping, range parsing, chunk arithmetic, config env
// fallback) lives as fast unit tests in the respective driver / server
// / chunk / config packages. The scenarios here are reserved for
// behavior that can only be verified end-to-end against real
// LocalStack (or Azurite, in azure_test.go) plus a real cluster of
// in-process orca instances.

// TestColdAndWarmGet exercises GET twice for the same single-chunk
// blob: cold (origin fetch + cache commit) and warm (cachestore hit).
// The warm phase deletes the origin object first to prove the cache
// hit really happened.
func TestColdAndWarmGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	cold := cl.Get(1).HTTP.Get(ctx, t, bucket, blob.Key)
	if cold.Status != http.StatusOK {
		t.Fatalf("cold status=%d body=%s", cold.Status, string(cold.Body))
	}

	if !bytes.Equal(cold.Body, blob.Data) {
		t.Fatalf("cold body mismatch: got %d bytes, want %d", len(cold.Body), len(blob.Data))
	}

	if cold.Header.Get("ETag") == "" {
		t.Errorf("expected ETag header on cold GET")
	}

	DeleteS3Object(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, blob.Key)

	warm := cl.Get(1).HTTP.Get(ctx, t, bucket, blob.Key)
	if warm.Status != http.StatusOK {
		t.Fatalf("warm status=%d body=%s", warm.Status, string(warm.Body))
	}

	if !bytes.Equal(warm.Body, blob.Data) {
		t.Fatalf("warm body mismatch: got %d bytes, want %d", len(warm.Body), len(blob.Data))
	}
}

// TestRangedGet verifies byte-range requests return 206 +
// Content-Range + the requested slice. Covers within-chunk,
// cross-chunk, and (against a 64-chunk blob) various boundary edge
// cases. The chunk-arithmetic branches are unit-tested separately in
// internal/orca/chunk; this verifies the end-to-end HTTP Range
// round-trip with real chunk bodies.
func TestRangedGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	medium := MediumBlob() // 1.5 MiB == 2 chunks at 1 MiB
	huge := HugeBlob()     // 64 MiB == 64 chunks at 1 MiB
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{medium, huge})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	resp := cl.Get(1).HTTP.GetRange(ctx, t, bucket, medium.Key, 100, 199)
	if resp.Status != http.StatusPartialContent {
		t.Fatalf("status=%d (want 206)", resp.Status)
	}

	if cr := resp.Header.Get("Content-Range"); cr == "" {
		t.Errorf("expected Content-Range header")
	}

	want := medium.Data[100:200]
	if !bytes.Equal(resp.Body, want) {
		t.Fatalf("range body mismatch: got %d bytes, want %d", len(resp.Body), len(want))
	}

	chunkSize := int64(1024 * 1024)
	resp2 := cl.Get(1).HTTP.GetRange(ctx, t, bucket, medium.Key, chunkSize-50, chunkSize+49)

	if resp2.Status != http.StatusPartialContent {
		t.Fatalf("cross-chunk status=%d (want 206)", resp2.Status)
	}

	want2 := medium.Data[chunkSize-50 : chunkSize+50]
	if !bytes.Equal(resp2.Body, want2) {
		t.Fatalf("cross-chunk range mismatch: got %d bytes, want %d", len(resp2.Body), len(want2))
	}

	t.Run("huge blob boundary cases", func(t *testing.T) {
		const chunk = int64(1024 * 1024)

		cases := []struct {
			name       string
			start, end int64
		}{
			{"starts exactly at chunk boundary 32", 32 * chunk, 32*chunk + 100},
			{"ends exactly at chunk boundary 47", 48*chunk - 100, 48*chunk - 1},
			{"covers chunks 10-12 (3 contiguous full chunks)", 10 * chunk, 13*chunk - 1},
			{"straddles 5 consecutive boundaries (chunks 20-25)", 20*chunk + 100, 25*chunk + 200},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rr := cl.Get(1).HTTP.GetRange(ctx, t, bucket, huge.Key, tc.start, tc.end)
				if rr.Status != http.StatusPartialContent {
					t.Fatalf("status=%d (want 206)", rr.Status)
				}

				expected := huge.Data[tc.start : tc.end+1]
				if !bytes.Equal(rr.Body, expected) {
					t.Fatalf("body mismatch: got %d bytes, want %d", len(rr.Body), len(expected))
				}
			})
		}
	})
}

// TestMultiChunkGet verifies a full GET of a 64-chunk blob assembles
// correctly across chunk boundaries. With 3 replicas and 64 chunks,
// rendezvous-hashed coordinator selection statistically guarantees
// every replica is the coordinator for many chunks, so this test
// exercises both fillLocal and FillFromPeer paths thoroughly in a
// single run.
func TestMultiChunkGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := HugeBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	resp := cl.Get(1).HTTP.Get(ctx, t, bucket, blob.Key)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, string(resp.Body))
	}

	if !bytes.Equal(resp.Body, blob.Data) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(resp.Body), len(blob.Data))
	}
}

// TestRendezvousCoordinatorRouting verifies that a GET against a
// non-coordinator replica routes through /internal/fill to the
// coordinator and still returns the body. The CountingOrigin
// decorator confirms exactly one origin GetRange happened across the
// cluster (the coordinator's).
func TestRendezvousCoordinatorRouting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	count := newCountingOriginForLocalStack(ctx, t, bucket)

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:     pkgLocalStack,
		OriginBucket:   bucket,
		OriginOverride: count,
	})

	headResp := cl.Get(1).HTTP.Head(ctx, t, bucket, blob.Key)

	etag := stripQuotes(headResp.Header.Get("ETag"))
	if etag == "" {
		t.Fatalf("HEAD returned empty ETag: %+v", headResp.Header)
	}

	k := chunk.Key{
		OriginID:  "inttest-origin",
		Bucket:    bucket,
		ObjectKey: blob.Key,
		ETag:      etag,
		ChunkSize: int64(1024 * 1024),
		Index:     0,
	}
	coord := cl.Get(1).App.Cluster.Coordinator(k)

	var nonCoord *Replica

	for _, r := range cl.Replicas {
		if r.SelfIP != coord.IP || r.InternalPort != coord.Port {
			nonCoord = r
			break
		}
	}

	if nonCoord == nil {
		t.Fatalf("could not find a non-coordinator replica; coord=%+v peers=%+v",
			coord, cl.Get(1).App.Cluster.Peers())
	}

	count.Reset()

	resp := nonCoord.HTTP.Get(ctx, t, bucket, blob.Key)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, string(resp.Body))
	}

	if !bytes.Equal(resp.Body, blob.Data) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(resp.Body), len(blob.Data))
	}
	// Exactly one HEAD (HeadObject metadata cache) plus one GetRange
	// (single chunk fetch). Cluster-wide dedup must not produce more.
	if got := count.GetRanges(); got != 1 {
		t.Errorf("origin GetRange count=%d (want 1)", got)
	}
}

// TestSingleflightCollapse fires N concurrent GETs (one per replica)
// for the same key and asserts the origin saw exactly one GetRange
// per chunk (cluster-wide singleflight collapse).
func TestSingleflightCollapse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := HugeBlob() // 64 chunks
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	count := newCountingOriginForLocalStack(ctx, t, bucket)

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:     pkgLocalStack,
		OriginBucket:   bucket,
		OriginOverride: count,
	})

	count.Reset()

	var wg sync.WaitGroup

	wg.Add(cl.Len())

	results := make([][]byte, cl.Len())
	statuses := make([]int, cl.Len())

	for i := 1; i <= cl.Len(); i++ {
		go func(i int) {
			defer wg.Done()

			r := cl.Get(i).HTTP.Get(ctx, t, bucket, blob.Key)
			results[i-1] = r.Body
			statuses[i-1] = r.Status
		}(i)
	}

	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("replica %d status=%d", i+1, s)
		}

		if !bytes.Equal(results[i], blob.Data) {
			t.Fatalf("replica %d body mismatch: got %d bytes want %d", i+1, len(results[i]), len(blob.Data))
		}
	}
	// HugeBlob spans 64 chunks; cluster-wide singleflight should
	// dedupe each chunk to exactly one origin GetRange. Allow up to
	// 76 (~20% slack) to absorb timing-dependent races where a
	// joiner arrives during in-flight commit.
	if got := count.GetRanges(); got > 76 {
		t.Errorf("origin GetRange count=%d (want <= 76 for 64-chunk blob)", got)
	}

	if got := count.GetRanges(); got < 64 {
		t.Errorf("origin GetRange count=%d (want >= 64 for 64-chunk cold fill)", got)
	}
}

// TestPeerNotCoordinatorFallback induces real membership disagreement
// and asserts the coordinator's /internal/fill returns 409 and the
// requesting replica's local-fill fallback succeeds.
//
// Setup:
//
//   - 3-replica cluster with shared CountingInternalHandlerWrap so we
//     can read 409 counts per receiving replica.
//   - HEAD the seeded blob to learn ETag; compute Coordinator(k) for
//     chunk 0 from replica 1's view (call it C).
//   - Craft a phantom peer P (an unreachable IP/Port pair) whose
//     rendezvous score for k is higher than C's. Mutate C's peer
//     source to include P plus C itself; now C.IsCoordinator(k)
//     returns false because P wins.
//   - Find another replica R whose view still says C is the
//     coordinator. GET via R.
//
// Expected:
//
//   - R issues /internal/fill to C.
//   - C responds 409 (its IsCoordinator returns false because P wins).
//   - R falls through to fillLocal, fetches the origin, serves the
//     body.
//   - counter.Count(C, 409) >= 1.
func TestPeerNotCoordinatorFallback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	wrap := NewCountingInternalHandlerWrap()

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:          pkgLocalStack,
		OriginBucket:        bucket,
		InternalHandlerWrap: wrap,
	})

	headResp := cl.Get(1).HTTP.Head(ctx, t, bucket, blob.Key)

	etag := stripQuotes(headResp.Header.Get("ETag"))
	if etag == "" {
		t.Fatalf("HEAD returned empty ETag: %+v", headResp.Header)
	}

	k := chunk.Key{
		OriginID:  "inttest-origin",
		Bucket:    bucket,
		ObjectKey: blob.Key,
		ETag:      etag,
		ChunkSize: int64(1024 * 1024),
		Index:     0,
	}
	coord := cl.Get(1).App.Cluster.Coordinator(k)

	coordReplica := cl.FindBySelfIPPort(coord.IP, coord.Port)
	if coordReplica == nil {
		t.Fatalf("coord %+v not found among replicas", coord)
	}

	// Craft a phantom peer whose rendezvous score beats coord's for k.
	// The phantom's IP/Port don't need to be reachable; it's never
	// dialed, only used to skew rendezvous on coord's view.
	pathBytes := []byte(k.Path())
	coordScore := cluster.Score(coord, pathBytes)
	phantom := cluster.Peer{IP: "203.0.113.1"} // TEST-NET-3, unreachable

	for port := 1; port < 65536; port++ {
		phantom.Port = port
		if cluster.Score(phantom, pathBytes) > coordScore {
			break
		}
	}

	if cluster.Score(phantom, pathBytes) <= coordScore {
		t.Fatalf("could not find a phantom peer beating coord rendezvous score")
	}

	// Build coord's new peer-set: original real peers plus the
	// phantom. The StaticPeerSource will stamp Self=true only on the
	// peer matching coord's (selfIP, selfPort), so coord still
	// recognizes itself; but the phantom wins rendezvous, so
	// coord.IsCoordinator(k) flips to false.
	newPeers := make([]cluster.Peer, 0, cl.Len()+1)
	for _, r := range cl.Replicas {
		newPeers = append(newPeers, cluster.Peer{IP: r.SelfIP, Port: r.InternalPort})
	}

	newPeers = append(newPeers, phantom)
	coordReplica.PeerSource.SetPeers(newPeers)

	if err := waitForCondition(ctx, 2*time.Second, func() bool {
		return !coordReplica.App.Cluster.IsCoordinator(k)
	}); err != nil {
		t.Fatalf("coord did not relinquish coordinator status: %v", err)
	}
	// Find a replica R whose view still says coord is the coordinator.
	var requester *Replica

	for _, r := range cl.Replicas {
		if r == coordReplica {
			continue
		}

		rc := r.App.Cluster.Coordinator(k)
		if rc.IP == coord.IP && rc.Port == coord.Port {
			requester = r
			break
		}
	}

	if requester == nil {
		t.Fatalf("no non-coord replica still views coord %+v as coordinator", coord)
	}

	resp := requester.HTTP.Get(ctx, t, bucket, blob.Key)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, string(resp.Body))
	}

	if !bytes.Equal(resp.Body, blob.Data) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(resp.Body), len(blob.Data))
	}

	coordKey := coord.IP + ":" + strconv.Itoa(coord.Port)
	if got := wrap.Count(coordKey, http.StatusConflict); got < 1 {
		t.Fatalf("expected at least one 409 from coord %s; got %d",
			coordKey, got)
	}
}

func newCountingOriginForLocalStack(ctx context.Context, t *testing.T, bucket string) *CountingOrigin {
	t.Helper()

	or, err := localStackOrigin(ctx, t, bucket)
	if err != nil {
		t.Fatalf("localStackOrigin: %v", err)
	}

	return NewCountingOrigin(or)
}

func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}

	return s
}

func waitForCondition(ctx context.Context, dl time.Duration, cond func() bool) error {
	deadline := time.Now().Add(dl)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}

	if cond() {
		return nil
	}

	return context.DeadlineExceeded
}
