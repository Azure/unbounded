---
title: "Profile Racer Dataplane Heap Memory"
weight: 9
description: "Build an opt-in heap profiler and collect Racer allocation profiles with Parca."
---

Racer dataplane can expose sampled live Rust heap allocations as gzipped pprof
profiles for Parca. This is separate from Parca Agent's CPU sampling and from
Racer's existing health and metrics listener.

## Build a profiling image

The default binary and image do not include a profiling allocator. Enable the
optional `heap-profiling` Cargo feature, or build the container with:

```sh
docker build -f images/racer-dataplane/Containerfile \
  --build-arg RACER_HEAP_PROFILING=true \
  -t racer-dataplane:heap .
```

This selects jemalloc, installs libunwind, and retains release debug information
for in-process symbolization. It also works with `RACER_NATIVE_RDMA=true`.
Local Rust builds require libunwind development headers and libraries:

```sh
CARGO_PROFILE_RELEASE_DEBUG=1 cargo build --locked --release \
  --manifest-path cmd/racer-dataplane/Cargo.toml --features heap-profiling
```

Profiling builds use jemalloc even when sampling is off. Ordinary builds retain
their existing allocator and dependencies. Test a profiling image on a canary
before using it broadly: allocation sampling and symbolization have overhead.

## Enable collection at process startup

Set these environment variables **before launching** the profiling binary:

```text
RACER_HEAP_PROFILE_ADDR=127.0.0.1:6060
_RJEM_MALLOC_CONF=prof:true,prof_active:true,lg_prof_sample:19
TMPDIR=/run/racer-heap
```

Create `TMPDIR` first, writable only by the dataplane UID (65532 in the image).
The allocator reads its configuration before `main`; changing the environment
later will not enable sampling. `lg_prof_sample:19` samples an average of one
allocation per 512 KiB allocated. There is no HTTP activation endpoint.

The address must be a numeric IP and a nonzero port. Unset
`RACER_HEAP_PROFILE_ADDR` to avoid creating a listener. Configuring it on a
non-profiling build, with inactive sampling, or with an unavailable bind address
fails startup. Temporary-file availability is checked at dump time, not startup.

The endpoint has **no authentication or TLS**. Keep it on loopback for local
inspection. For remote Parca scrapes, choose a reachable bind address and restrict
access with the network controls appropriate to your deployment. Profiles expose
allocation stacks, executable and source paths, and code metadata. They are not
raw heap-payload dumps, but should still be treated as sensitive.

### Kubernetes considerations

This feature does not automatically modify operator-generated workloads or Parca
configuration. Arrange the image, environment, and writable temporary storage in
your deployment configuration before rolling out a canary. The generated Racer
dataplane uses a read-only root filesystem and has no writable `/tmp` mount by
default, so setting only the two profiling environment variables is insufficient.
An example container/volume fragment is:

```yaml
# Merge into the dataplane container specification.
env:
  - name: RACER_HEAP_PROFILE_ADDR
    value: "127.0.0.1:6060"
  - name: _RJEM_MALLOC_CONF
    value: "prof:true,prof_active:true,lg_prof_sample:19"
  - name: TMPDIR
    value: /run/racer-heap
volumeMounts:
  - name: heap-profile-tmp
    mountPath: /run/racer-heap
---
# Merge into the Pod specification. Ensure the mount is writable by UID 65532
# using an appropriate volume ownership setup; restrict directory permissions.
volumes:
  - name: heap-profile-tmp
    emptyDir:
      sizeLimit: 128Mi
```

Size temporary storage for your workload and monitor usage. `emptyDir.sizeLimit`
is not a synchronous per-write quota; it does not bound dump memory usage.
Do not put profiles in the identity or cache directories. A loopback listener is
not reachable from a remote Parca pod. A wildcard listener on a host-networked
dataplane exposes the node port; do not rely on pod NetworkPolicy alone to protect
host-network traffic. Direct edits to operator-managed pods are not durable.

## Inspect a profile

```sh
curl --fail --max-time 65 http://127.0.0.1:6060/debug/pprof/allocs -o heap.pb.gz
go tool pprof -top heap.pb.gz
```

`GET /debug/pprof/heap` is an alias. Both routes return the same gzip-compressed
protobuf with `Content-Type: application/octet-stream` and `Cache-Control:
no-store`. Do not add `seconds`, `gc`, or other query parameters. Only plain GET
snapshot requests are supported.

## Configure Parca

Merge this scrape job into the Parca server configuration, preserving the
existing object-storage settings. Replace the target with the reachable,
access-controlled address of a profiling dataplane:

```yaml
scrape_configs:
  - job_name: racer-heap
    scheme: http
    scrape_interval: 60s
    scrape_timeout: 65s
    normalized_addresses: true
    profiling_config:
      pprof_config:
        memory:
          enabled: true
          path: /debug/pprof/allocs
          delta: false
          keep_sample_type:
            - type: inuse_space
              unit: bytes
        process_cpu:
          enabled: false
        block:
          enabled: false
        goroutine:
          enabled: false
        mutex:
          enabled: false
    static_configs:
      - targets: ["racer-profile.example.internal:6060"]
```

These settings follow Parca v0.28's scrape configuration: memory snapshots use
`/debug/pprof/allocs`, the other default profile types must be disabled explicitly,
and the scrape timeout must exceed the interval. `normalized_addresses: true`
matches the profiler's file-relative addresses. No Parca Agent change is needed.
After applying the configuration using your usual Parca deployment workflow,
select the memory profile with sample type `inuse_space` and unit `bytes`, filtered
by `job="racer-heap"`.

## Interpretation and limits

- The profile estimates **currently live sampled allocations** across threads. It
  is not RSS, total mapped memory, slab-file capacity, cumulative allocation
  traffic, or object counts. Retained cache/buffer allocations are not necessarily
  leaks. Allocations outside the Rust global allocator may not appear.
- A single dedicated thread handles one connection/dump at a time, independently
  of the reactor's health/metrics path. Other scrapes wait in the socket backlog;
  there is no per-request thread pool. Budget an additional thread and avoid
  overlapping collectors.
- Requests are limited to 8 KiB and 64 headers, with two-second read and write
  deadlines. Encoded responses exceeding 32 MiB return HTTP 500, never truncated
  profiles. Unavailable temporary storage and conversion errors also return 500.
- The response ceiling does **not** bound the upstream native dump, symbolization
  memory, temporary-file size, or duration. A client timeout cannot cancel native
  work. Start with infrequent scrapes and watch dataplane resource usage.
- Shutdown signals the profiling worker to stop without joining an in-progress
  native dump; process exit terminates it. Abrupt termination can leave temporary
  files, so use disposable storage rather than a persistent sensitive directory.
