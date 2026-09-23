# racer-object verification

Run from the repository root. Use an existing, short workspace-local scratch
directory for Unix sockets. A configured but unusable integration prerequisite
fails its test; missing opt-in variables cause explicit skips.

## Required local checks

```sh
export TMPDIR="$PWD/tmp"
make fmt GO_PACKAGE_DIRS='cmd/racer-object pkg/racer' \
  GO_PACKAGE_PATTERNS='./cmd/racer-object/... ./pkg/racer/...'
go test -race ./cmd/racer-object ./pkg/racer
make racer-object-build image-racer-object-local
python3 -B cmd/racer-object/verify_splice.py \
  --binary bin/racer-object --scratch "$TMPDIR"
```

The syscall check needs Linux `strace` and traces only the production frontend.
Its separate Unix fixture sends headers and body together. Two persistent GETs
must return exact bytes; successful splice counts must equal twice the total
body bytes (both pipe legs), and the payload marker must never appear in any
frontend read/write/send/receive syscall. The Go header-reader test independently
rejects any read buffer that could cross the header terminator, including every
tested fragmentation boundary.

Socket tests cover ranges across 4 MiB boundaries, HEAD, conditional 304,
keep-alive, unsupported operations, framing/version/range rejection, truncated
bodies, deadlines, downstream disconnect, backpressure and shutdown. Backend
tests check one metadata lookup and one Azure GET per read-ahead page, bounded
admission, cancellation, cleanup, Azure response validation, and mandatory
Workload Identity configuration without credential fallback.

## Real Racer cold/warm path

```sh
make racer-object-build racer-dataplane-build
RACER_OBJECT_BINARY="$PWD/bin/racer-object" \
RACER_DATAPLANE_BINARY="$PWD/bin/racer-dataplane" \
  go test -v ./pkg/racer -run '^TestObjectDataplaneInterop$' -timeout 120s
```

This starts both production object modes, the current Rust daemon, a test mTLS
control plane and an Azure protocol fixture. It asserts exact bytes on cold and
warm GETs, one Azure HEAD, two cold page GETs and zero additional warm Azure
requests. Build the current daemon: stale binaries can have incompatible control
protocols. See the dataplane README for io_uring, ext4 and locked-memory requirements.

To check an existing deployment with a real Azure origin:

```sh
RACER_OBJECT_CACHE_SOCKET=/dev/racer/weights/cache \
RACER_OBJECT_CONFIG=/config/objects.json \
RACER_OBJECT_SHA256=EXPECTED_COMPLETE_OBJECT_SHA256 \
  go test -v ./cmd/racer-object -run '^TestExternalCache$'
```

That test downloads the first configured object twice and checks bytes and splice
accounting. Use deployment metrics to distinguish warm hits from repeated origins.

## Real Run:ai and vLLM

Build the existing `images/racer-vllm-client/Containerfile` image, which pins
vLLM 0.29.0 (CPU build) and Run:ai 0.16.1, then run:

```sh
RACER_OBJECT_RUNAI_IMAGE=racer-vllm-client:test \
  go test -v ./cmd/racer-object -run '^TestRunaiExplicitWeights$' -timeout 180s
```

Docker host networking is required. The test generates actual safetensors, loads
parameters through the native S3 streamer and production frontend transport,
checks tensors and linear inference, and exercises the explicit-file loader with
discovery patched to fail. This is CPU loader integration, not GPU serving or
tensor-parallel performance coverage.

## Live AKS identity

Run the following inside a pod with the example ServiceAccount, webhook label,
federated identity and Blob Data Reader role. The digest covers the first 4 MiB
(or the complete file if smaller) of the first configured object.

```sh
RACER_OBJECT_AKS_CONFIG=/config/objects.json \
RACER_OBJECT_AKS_PAGE_SHA256=EXPECTED_FIRST_PAGE_SHA256 \
  go test -v ./cmd/racer-object -run '^TestLiveWorkloadIdentity$'
```

This requires real Azure access; local configuration/HTTP fixtures do not prove
AKS federation, role assignments, or projected-token rotation. Long-lived token
rotation should also be exercised in the target cluster.

## Performance

```sh
go test ./cmd/racer-object -run '^$' \
  -bench 'Benchmark(FrontendSplice|DirectOrigin)$' -benchtime=2s -cpu=8 -benchmem
```

Both fixtures transfer 16 MiB objects from the same in-memory backend. The direct
baseline serves the origin over TCP; the frontend fixture adds Unix transport and
the splice bridge. Allocation figures include origin and consumer work.

On the implementation host (Linux amd64, AMD EPYC 9V74, eight Go execution
threads), one run measured 6.76 GB/s through the frontend and 9.39 GB/s direct,
with about 87 KB and 69 KB allocated per request respectively. These are local
fixture measurements, not Azure bandwidth or production guarantees. Measure cold
Azure and warm Racer separately in deployment, including process CPU/RSS and
request counts; increasing client concurrency multiplies active kernel pipes.

Implementation validation passed local race/lint checks, the syscall proof,
the real-daemon cold/warm test, image build and the pinned CPU loader test.
Live AKS federation/rotation and GPU serving require target-environment validation.
