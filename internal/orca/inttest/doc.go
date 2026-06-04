// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

// Package inttest contains integration tests for the Orca cache.
//
// Build tag `integrationtest` gates these tests; run via:
//
//	make orca-inttest
//
// Equivalent to:
//
//	go test -tags=integrationtest -race -timeout 15m \
//	  ./internal/orca/inttest/...
//
// # Architecture
//
// The harness brings up a real S3-compatible backend and Azurite containers via
// testcontainers-go and constructs N in-process *app.App instances
// wired to those containers. By default StartCluster runs 3 replicas,
// matching the production deploy/orca topology.
//
// Every replica binds to 127.0.0.1 with an OS-assigned distinct
// internal port; the cluster.Peer struct now carries an explicit Port
// (zero in production, set in tests) and FillFromPeer dials peer.IP +
// peer.Port. This lets multi-replica tests run on every platform
// (Linux, macOS, Windows / WSL) without loopback-alias setup.
//
// Each replica owns its own StaticPeerSource (cluster.PeerSource).
// Tests that need to induce membership disagreement mutate one
// replica's source; the cluster's refresh goroutine picks up the
// change within MembershipRefresh (250 ms in tests).
//
// # Container lifecycle
//
// TestMain starts one S3 backend and one Azurite container per
// `go test` invocation; per-test buckets/containers prevent
// cross-test interference.
//
// # File layout
//
//   - e2e_test.go - the canonical end-to-end suite (3 replicas).
//     Boot-self-test, cold/warm GET, ranged GET, multi-chunk GET,
//     LIST, HEAD, NotFound, rendezvous coordinator routing,
//     singleflight collapse, peer-not-coordinator fallback (real).
//   - azure_test.go - azureblob origin driver smoke against Azurite
//     (3 replicas).
//
// Driver-level branch coverage (versioning gate, blob-type
// rejection) lives as fast unit tests in the respective driver
// packages (cachestore/s3, origin/azureblob), not here.
//
// # Adding a scenario
//
//  1. Pick the right entry point: StartCluster (3-replica default).
//     Tests that need to assert on a boot-time failure mode that
//     surfaces before any chunk fetch (versioning gate, blob-type
//     rejection, etc.) should live as unit tests in the respective
//     driver package.
//  2. Seed the origin: SeedS3 or SeedAzure.
//  3. Issue requests via cl.Get(i).HTTP.Get / GetRange / Head / List.
//  4. Assert byte-exact body, status code, and (where relevant) origin
//     RPC counts via the optional CountingOrigin or peer 409 counts via
//     CountingInternalHandlerWrap.
//
// # TODO (genuinely future work)
//
//   - TestEtagChange (mid-fill mutation): requires a deterministic
//     test seam in fetch.Coordinator (e.g. a hook that pauses between
//     chunk fetches) so the test can rewrite the origin object
//     between chunk 0 and chunk 1 of the same fill.
//   - Fault-injection origin / cachestore decorators: useful for
//     timeout, throttle, and 5xx retry-budget assertions.
package inttest
