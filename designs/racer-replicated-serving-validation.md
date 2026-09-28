# Replicated serving: bounded real-TLS smoke validation

## Scope and result (2026-09-28)

**1,000 and 10,000 distinct HTTPS protocol clients passed on one host. 100,000
was not run and is not a capacity claim.** These are actual TCP/TLS 1.3
connections with distinct Ed25519 keys, certificates, and node URI identities,
not synthetic `Publications.Wait` calls. They are Go test clients, **not 10,000
running Rust dataplane processes**. The full production dataplane-fleet target
remains unmeasured, as does Kubernetes API authorization capacity.

The harness is `internal/racer/replication_scale_test.go`. Its ordinary
`TestReplicatedServingSmoke` uses 12 clients; the capacity test skips unless
`RACER_REPLICATION_CLIENTS` specifies one exact count from 1 through 10,000.
There is no automatic size ladder, unbounded retry, new dependency, host setting
change, envtest startup, or cluster mutation.

### What is exercised

- Three independent application/publication/lifecycle instances, sharing a fake
  Kubernetes authority. Each listener uses production `Server.serve`, including
  TLS configuration, HTTP admission, connection cancellation, and shutdown
  (`internal/racer/server.go:67`, `:131`, `:419`). Assignment is deterministic
  round-robin, **not a measurement of Kubernetes Service load balancing**.
- One dedicated HTTP/1.1 transport per node, at most one connection per host,
  server certificate verification, and real client-certificate verification.
  Snapshot authentication revalidates against local trust, not the Kubernetes
  API (`internal/racer/certificates.go:285`). Certificates are issued locally
  from fixture signing material, without a fleet bootstrap/enrollment storm.
- A full publication with one member per client, distinct peer endpoints, empty
  rails and an empty cache catalog. Every member's shares change on each update.
  Prepare, canonical encoding, fake durable CAS, and Install run normally
  (`internal/racer/publications.go:155`, `internal/racer/version.go:213`). This
  smaller fixture is not the 16-cache/two-rail fixture in `scale_test.go:96`.
- The production `/internal/v1/snapshot` TLS route sends full publications to
  both followers. TokenReview is mocked, but its requested audience is checked,
  and the production controller Pod/ServiceAccount authorization checks run
  against fake objects (`internal/racer/replication.go:328`). Each follower
  decodes, hashes, confirms the fake durable record, and installs normally
  (`internal/racer/replication.go:192`). Initial synchronization makes two mock
  TokenReviews, the first update two more, the post-failure update one more.
  Missing public certificates and missing internal bearer tokens are rejected.
- Controller selection and replication scheduling are test-local. This does
  **not** run `Replication.poll`'s Lease discovery, token/CA-file reload, or
  background jitter loop (`internal/racer/replication.go:232`, `:263`). The
  full-image fanout and validated follower installation are real, but observed
  replication latency excludes production polling intervals and real API latency.
- Five-second observations of fake authority preserve the default 30-second
  freshness window, rather than disabling freshness. A test-local publisher
  prepares updates; controller-runtime caches and election are not started.
- After cold delivery, all clients park real long polls at the current sequence.
  The harness checks that every request is admitted before publishing. Each
  client streams the complete response through SHA-256 and checks exact bytes
  against the canonical publication. It does not deserialize/store a full Rust
  membership table in every client. No deltas are requested.
- A second parked round cancels follower 2's serving process. Its requests may
  fail with a transport error or HTTP 503. A test-local failover decision moves
  those clients to replicas 0/1; the harness waits for all clients to park there
  before the next update. This is follower loss, **not publisher election or
  Service endpoint propagation**. All formerly connected clients must recover.

### Load shaping and bounds

Production admission defaults remain 128 simultaneous writes and 32 local
authentication slots per replica (`internal/racer/config.go:98`). Cold requests
are limited to 24 in flight, and new TLS handshakes (including failover) to 24
globally. Already-established polls all wake together. HTTP 429 responses use
1.000-1.899 seconds of deterministic jitter, respecting the one-second retry
floor. There are at most six application attempts per request. Unexpected
statuses/errors fail; failed-replica transport errors/503s are the only failover
exceptions. Go's HTTP transport can also retry an idempotent request internally.

The test has a 240-second context, 45-second request bounds, ten-second TLS
handshake bounds, and a 20-second parked-request barrier (less than the production
30-second long-poll timeout). It cancels and joins workers and closes listeners
and idle transports. Every command below has an external 300-second TERM bound
with a ten-second kill fallback. No unbounded retries are appropriate on failure.

## Environment assessment

Before load: Linux amd64, Go 1.27.1, 48 logical CPUs, 94 GiB RAM, approximately
67 GiB available, no swap, load average 5.88/6.81/8.01, FD soft limit 1,048,576.
The process cgroup and its user ancestors had `memory.max=max`,
`memory.high=max`, and `cpu.max=max 100000`; process-scope pids limit was 107,414.
Initial process-scope memory usage was about 3.78 GB (includes other scope work).
Filesystem had 35 GiB available. All measurements used `GOMAXPROCS=8` to avoid
occupying the host's full CPU allotment. These are shared-host samples, not
isolated benchmark statistics.

Initial resource commands, all wrapped in the same external timeout, included
`nproc`, `free -h`, `ulimit -n`, `uptime`, `df -h .`, `go version`, reads of
`/proc/self/cgroup`, `/proc/net/sockstat`,
`/proc/sys/net/ipv4/ip_local_port_range`, and the actual process cgroup/ancestor
`memory.{max,high,current}`, `cpu.max`, and `pids.{max,current}` files. Root-level
and legacy cgroup-v1 limit files were absent; the assessment used the actual
cgroup-v2 path rather than interpreting absence as an unlimited process.

The ephemeral range was 32768-60999: 28,232 source ports per destination tuple.
100,000 loopback clients spread over only three destinations would require
33,333 or more each, and 50,000 after one failure. That cannot be a valid local
test without changing addressing or host settings. No such changes were made.
Furthermore full-image traffic grows quadratically with this fleet-size fixture:
the 10k run delivered 13.03 GB per round. A 100k run would approach 1.3 TB per
round with this schema, before TLS framing, retries, and client decode costs.
This is an inference from fixture size, not a 100k measurement.

## Recorded successful measurements

One successful non-race sample per size is recorded below. Durations are not
capacity guarantees or repeated-run percentiles.

| Measurement | 1,000 clients | 10,000 clients |
| --- | ---: | ---: |
| Fixture setup | 0.141 s | 1.313 s |
| Full publication bytes | 129,693 | 1,303,257 |
| Cold delivery, all clients | 0.361 s | 6.149 s |
| Cold request p50 / p99 | 6.365 / 30.756 ms | 12.777 / 43.247 ms |
| Parked requests by replica | 334 / 333 / 333 | 3334 / 3333 / 3333 |
| First update, prepare through last delivery | 0.110 s | 5.331 s |
| First update request p50 / p99 | 95.689 / 139.985 ms | 2.557 / 4.709 s |
| Follower transfer/install, first update | 29.811 ms | 274.290 ms |
| Failure round start through all re-parked | 95.482 ms | 913.006 ms |
| Recovered clients | 333 | 3,333 |
| Parked requests after failure | 501 / 499 / 0 | 5001 / 4999 / 0 |
| Post-failure update through last delivery | 0.113 s | 4.941 s |
| Post-failure request p50 / p99 | 157.502 / 208.158 ms | 3.509 / 5.806 s |
| Follower transfer/install, post-failure | 8.808 ms | 101.274 ms |
| HTTP 429s: first update / post-failure | 0 / 0 | 186 / 74 |
| Successful application bytes, each round | 129,693,000 | 13,032,570,000 |
| Total test time reported by Go | 0.95 s | 19.23 s |

`all_delivered` for updates starts **before Prepare/fake CAS**, so includes
publication construction, leader installation, follower transfer/validation, and
client delivery/retries. Request p50/p99 include pre-publication parking; they
are not pure update propagation percentiles. Cold request latencies exclude the
24-slot admission queue, while cold all-delivered includes it. The failure-repark
metric includes initial admission of the second parked round, not just time
after cancellation. Replication is sequential across followers in this harness.

Resource samples cover **clients, all controllers, fake API, and test runtime in
one process**; they cannot establish a per-controller production memory budget.

| Resource | 1,000 clients | 10,000 clients |
| --- | ---: | ---: |
| First parked heap bytes | 152,972,192 | 1,262,518,984 |
| First parked Go stack bytes | 38,141,952 | 370,671,616 |
| First parked RSS (KiB) | 236,936 | 2,224,768 |
| Largest logged VmHWM (KiB) | 345,332 | 3,370,424 |
| First parked goroutines | 5,012 | 50,012 |
| First parked process FDs | 2,012 | 20,012 |
| OS threads at first parked sample | 13 | 12 |
| Final cumulative Go allocated bytes | 1,020,569,104 | 45,246,456,216 |

At 10k the initial live TCP distribution was `[3335,3333,3333]`, including one
internal replication connection on replica 0. After failure it was
`[5002,4999,0]`; cumulative accepted connections were `[5002,4999,3333]`.
Successful cold and first-update counts did not require new dataplane TCP
connections, demonstrating connection reuse. Sampling does not capture all
transient peaks; `/proc` stats are Linux-specific and Go allocation totals are
cumulative, not retained memory.

### Failures and validation status

Two earlier 1k development runs failed their 20-second re-parking barrier
(718/1000 and 707/1000). The first harness only recovered transport errors and
missed shutdown HTTP 503 responses. Limiting failover handshakes alone did not
fix it. Handling the explicit failed-replica 503/body-error case fixed recovery;
the successful samples above are from that corrected harness. They are not
claims of an unrestricted reconnect storm passing production admission.

The ordinary 12-client mode also passed with the race detector after final
formatting and addition of the missing-credential checks. The exact one-client
edge case passed, and the focused `^Test(Replicated|Replica)` suite passed in
10.245 seconds (the capacity test skips without opt-in). Existing full
correctness/envtest results are separate evidence and are not counted as new
capacity measurements here. No envtest or live cluster was run by this harness.

## Reproduction

Assess available memory, FD limits, port availability, and CPU sharing before
choosing **one** count. Do not run capacity commands concurrently. The 12-client
mode is the default correctness coverage; use 1,000 before considering 10,000.

```sh
timeout --signal=TERM --kill-after=10s 300s env GOMAXPROCS=8 \
  go test ./internal/racer -run '^TestReplicatedServingSmoke$' \
  -race -count=1 -timeout=5m -v

timeout --signal=TERM --kill-after=10s 300s env GOMAXPROCS=8 \
  RACER_REPLICATION_CLIENTS=1000 \
  go test ./internal/racer -run '^TestReplicatedServingCapacity$' \
  -count=1 -timeout=5m -v

timeout --signal=TERM --kill-after=10s 300s env GOMAXPROCS=8 \
  RACER_REPLICATION_CLIENTS=10000 \
  go test ./internal/racer -run '^TestReplicatedServingCapacity$' \
  -count=1 -timeout=5m -v

timeout --signal=TERM --kill-after=10s 300s env GOMAXPROCS=8 \
  GOTOOLCHAIN=go1.26.6 make fmt \
  GO_PACKAGE_DIRS=internal/racer/replication_scale_test.go \
  GO_PACKAGE_PATTERNS=./internal/racer

timeout --signal=TERM --kill-after=10s 300s env GOMAXPROCS=8 \
  go test ./internal/racer -run '^Test(Replicated|Replica)' -count=1 -timeout=5m
```

The ambient Go 1.27 toolchain could run tests, but installed golangci-lint 2.11.4
was built with Go 1.26 and panicked when loading 1.27 sources. Running that linter
via Go 1.27 also failed because its export-data reader did not support version 4.
The scoped `make fmt` succeeded with the repository's declared Go 1.26.6
toolchain, with zero issues. A linter auto-fix incorrectly rewrote an assignment
to a nested lifecycle field as a short declaration; moving fixture lifecycle
initialization into a helper avoided that rewrite, and compilation/race tests
passed afterward. A concurrent linter invocation briefly held its lock; the
final scoped run used `GOLINT='golangci-lint run -c .golangci.yaml
--allow-serial-runners'` to wait within the command bound. No dependency or host
configuration was edited.

## External 100k follow-up: explicitly not measured

Do not substitute ten independent 10k tests: that measures ten disconnected
fixtures, not a 100k shared publication or controller fleet. The local harness
deliberately refuses counts above 10,000. No external load generator or manifest
is advertised here as if it already exists.

An external run needs an isolated, approved environment with real controller
replicas, provisioned serving CA and controller tokens, real Kubernetes authority,
and distributed source addresses. For the actual dataplane target, provision
100,000 real Rust dataplane instances with unique authorized Node/Pod identities;
otherwise label distributed protocol clients as such. Use the existing operator
deployment, not a test that patches an arbitrary current cluster. Required gates:

1. Check aggregate and per-host memory, descriptors, source ports, controller
   write/handshake admission, API budgets, and bandwidth. Keep enrollment/API
   authorization load separate from the established-certificate serving phase.
2. Establish one shared 100k-member publication and verify the number of actual
   authenticated live sockets/polls on every controller. Observe natural Service
   distribution rather than forcing the distribution used here.
3. Apply a versioned update in the isolated environment; collect full/delta bytes,
   per-client convergence p50/p99/max, 429s, failed attempts, API request rates,
   controller and client CPU/RSS/FDs, and replication lag. Hash/count evidence
   must include every intended client, not merely successful responses.
4. Separately test follower removal, publisher election, and load-balancer endpoint
   propagation. Record reconnect/election tails and freshness behavior.
5. Bound each orchestration phase to at most 300 seconds with TERM/kill cleanup;
   use resumable phases and explicit client progress counts for larger external
   runs. Stop on budget exhaustion or resource pressure. Record uncompleted
   clients as failures, not synthetic successes or an extrapolated capacity claim.
