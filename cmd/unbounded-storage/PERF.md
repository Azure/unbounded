# Custom TLS TCP RPC Performance Acceptance

This runbook is the performance acceptance procedure for the custom `tls_tcp`
RPC transport. It uses the existing `unbounded-storage` executable and its
in-process `loadgen` frontend. There is no separate benchmark executable and no
committed two-host automation wrapper.

`make unbounded-storage-smoke` is a loopback integration smoke test. It can catch
startup and correctness failures, but loopback bypasses the physical NIC and
cannot satisfy or substitute for this acceptance run.

## Acceptance Contract

Run two otherwise idle physical hosts connected through one 100Gb NIC per host.
Use warm memory, 2 MiB pages, and aggregate concurrent RPCs from Host A to Host B.
The acceptance threshold is **useful payload >=95 Gbps sustained on 100Gb NIC**
for one uninterrupted 60-second measurement window. Useful payload means page
body bytes only, not TLS, TCP, Ethernet, or RPC framing bytes.

Both hosts must use the same release binary and must have:

- CPU frequency policy, NUMA placement, MTU, NIC queues, IRQ affinity, and link
  negotiation recorded and held constant for the lane sweep.
- 2 MiB hugetlb pages reserved on every NUMA node used by serving shards.
  `[startup.memory].no_hugepages = false`; startup failure is preferable to a
  heap-backed acceptance run.
- A CA trusted by both hosts and one certificate/key per host. Set the peer name
  and `server_name` to the exact DNS SAN in that host's certificate. IP SANs and
  certificate common names do not replace the required DNS SAN.
- OpenSSL with TLS 1.3 and kTLS support, plus a kernel with the `tls` module.
  The daemon rejects a connection unless mutual authentication succeeds and
  kTLS is engaged for both TX and RX.

Before the run, confirm the NIC reports 100000 Mb/s and the configured interface
is the route to the other host. Confirm `/proc/meminfo` reports a 2048 kB hugepage
size and enough free hugepages for `memory_total_bytes` plus daemon scratch on
all serving shards. Record `lscpu`, `numactl --hardware`, `ethtool <iface>`,
`ethtool -k <iface>`, and `ethtool -l <iface>` with the result bundle.

## Configuration

Use this template on Host A. Replace the local certificate paths, self name,
addresses, disk path, and metrics address. Host B uses `self =
"node-b.storage.test"`, its own certificate/key and disk path, and the same peer
mesh, backend, cache, memory, fabric, metrics, and topology sections. Omit the
`[[frontends]]` loadgen section on Host B so traffic has one measured direction.
The certificate for each host must contain its peer name as a DNS SAN.

```toml
self = "node-a.storage.test"
fingers_per_node = 100

[[backends]]
name = "fake"

[backends.config.fake]
stripe_size_bytes = 2097152
object_size_bytes = 2097152

[[peers]]
name = "node-a.storage.test"

[peers.config.tls_tcp]
addr = "192.0.2.10:9443"
server_name = "node-a.storage.test"

[[peers]]
name = "node-b.storage.test"

[peers.config.tls_tcp]
addr = "192.0.2.11:9443"
server_name = "node-b.storage.test"

[[caches]]
name = "cache"
source = "fake"

[[disks]]
page_size_bytes = 2097152
skip_recovery_scan = true

[disks.config.file]
path = "/var/lib/unbounded-storage/perf.img"
size = 1073741824

[[frontends]]
name = "loadgen"
source = "cache"

[frontends.config.loadgen]
workers = 32
seed = 1234
keyspace_objects = 1000000
object_size_bytes = 2097152
read_bytes = 2097152
zipf_exponent = 1.1
verify = false
remote_only = true
fabric_only = true
skip_local_disk = true

[startup.memory]
no_hugepages = false
memory_total_bytes = 2147483648

[startup.fabric]
max_inflight = 8192

[startup.fabric.binds.tls_tcp]
addr = "0.0.0.0:9443"
ca_cert_path = "/etc/unbounded-storage/pki/ca.pem"
cert_path = "/etc/unbounded-storage/pki/node-a.storage.test.pem"
key_path = "/etc/unbounded-storage/pki/node-a.storage.test-key.pem"
lanes = 8
request_timeout_ms = 30000
socket_buffer_bytes = 16777216
ring_depth = 4096

[startup.metrics]
addr = "0.0.0.0:9100"

[startup.topology]
disable_rdma = true
use_smt_siblings = false
```

Host A's `fabric_only` load preserves routing, request framing, lane admission,
TLS/kTLS, fixed receives, and registered-source sends, while the serving peer
synthesizes zero-filled pages from RPC scratch instead of reading NVMe or origin. `remote_only` selects
objects whose first stripe routes to the peer. `skip_local_disk` removes
requester-side disk lookup/writeback. The million-object keyspace should remain
larger than the in-memory cache; verify during warmup that the TCP payload
counters continue increasing rather than flattening due to local cache hits.

The backing allocator always carves 2 MiB pages. The fake object, stripe, read,
and disk page sizes above make each successful body RPC carry one complete 2 MiB
page. `workers` is per serving shard. Increase it if needed so each shard can
keep every configured lane occupied; do not change it within a lane sweep.

## Build And Launch

Build once from the same revision on both hosts:

```bash
make unbounded-storage-build
```

Set `LIBFABRIC_VERSION` and `OPENSSL_VERSION` to the values used by the build,
then launch on each host. The explicit library path is required because the
binary still links libfabric for verbs/RDMA support and uses the pinned OpenSSL.

```bash
export LIBFABRIC_VERSION=2.5.1
export OPENSSL_VERSION=3.5.1
sudo -E env \
  LD_LIBRARY_PATH="$PWD/tmp/libfabric/$LIBFABRIC_VERSION/lib:$PWD/tmp/openssl/$OPENSSL_VERSION/lib" \
  "$PWD/bin/unbounded-storage" --config /etc/unbounded-storage/perf.toml
```

Use the actual Makefile overrides if the build did not use the shown defaults.
Capture each daemon PID and stderr. Wait until both metrics endpoints answer and
`unbounded_storage_tcp_rpc_active_connections` is nonzero on Host B. Host A owns
the outbound lanes, while this gauge counts accepted server connections. Any
TLS, DNS SAN, or kTLS failure invalidates the run rather than permitting
plaintext or userspace-TLS fallback.

## Lane Sweep

Sweep `lanes = 1, 2, 4, 8, 16, 32` in that order. `lanes` is startup-fixed, so
stop both daemons, change the value on both hosts, and restart both daemons for
every point. A lane is one persistent TCP connection per peer per shard and
admits one active request. With `S_A` requesting serving shards, the aggregate
concurrency ceiling is `S_A * lanes`, provided the per-shard loadgen worker count
and `max_inflight` are not lower.

For each point:

1. Start both daemons and allow exactly 60 seconds of unmeasured warmup. This
   faults the 2 MiB backing pages, establishes all lanes, warms code/data, and
   moves early connection failures outside the measured deltas.
2. Verify Host A's sent and Host B's received payload counters are increasing,
   RSS has plateaued, and no auth or protocol error counter changed during the
   final 10 seconds of warmup.
3. At the coordinated boundary, save `/metrics`, `nstat -az`, and
   `ethtool -S <iface>` on both hosts, note local wall-clock time `t0`, and start
   CPU collection.
4. Run for 60 uninterrupted seconds without config reloads, scraping, logging,
   or unrelated traffic beyond the planned measurements.
5. After the local 60-second CPU collector exits, note local wall-clock time
   `t1` and save `/metrics`, `nstat -az`, and `ethtool -S <iface>` again.
6. Compute and record throughput, request/error deltas, CPU, retransmits, lane
   waits, and active/inflight gauges. Keep every lane point, not only the best.

The following commands use existing daemon endpoints and standard Linux tools;
they are operator steps, not a repository harness:

```bash
PID=$(pgrep -n -x unbounded-storage)
IFACE=ens1f0
curl -fsS http://127.0.0.1:9100/metrics > /var/tmp/storage.before.prom
nstat -az > /var/tmp/nstat.before.txt
ethtool -S "$IFACE" > /var/tmp/ethtool.before.txt
date +%s.%N > /var/tmp/storage.t0
sudo perf stat -p "$PID" -e task-clock,cycles,instructions,context-switches,cpu-migrations \
  -o /var/tmp/storage.perf.txt -- sleep 60
date +%s.%N > /var/tmp/storage.t1
curl -fsS http://127.0.0.1:9100/metrics > /var/tmp/storage.after.prom
nstat -az > /var/tmp/nstat.after.txt
ethtool -S "$IFACE" > /var/tmp/ethtool.after.txt
ss -tin '( sport = :9443 or dport = :9443 )' > /var/tmp/storage.ss.txt
```

Run that block concurrently on both hosts, synchronized by PTP or NTP and an
operator-selected start boundary. The local `perf stat ... sleep 60` defines
each host's measurement interval; `t0` and `t1` expose launch skew. If `perf`,
`nstat`, or `ethtool` is unavailable, install the normal host tooling before
acceptance rather than omitting CPU, retransmit, or NIC evidence.

## Metrics And Formulas

Use these counters from each saved Prometheus snapshot:

- `unbounded_storage_tcp_rpc_payload_bytes_received_total`: useful page bytes
  received directly into fixed buffers. This is the acceptance numerator.
- `unbounded_storage_tcp_rpc_payload_bytes_sent_total`: independent cross-check.
  Across both hosts its delta must equal received bytes apart from requests cut
  by the exact measurement boundaries. Never add sent and received, which would
  double-count the same payload.
- `unbounded_storage_tcp_rpc_requests_total{outcome="ok|err"}`: completed server
  RPCs by result.
- `unbounded_storage_tcp_rpc_connection_errors_total`,
  `unbounded_storage_tcp_rpc_auth_errors_total`, and
  `unbounded_storage_tcp_rpc_protocol_errors_total`: failure deltas.
- `unbounded_storage_tcp_rpc_short_sends_total`: valid short registered-source
  completions retried by the transport; report the delta.
- `unbounded_storage_tcp_rpc_send_zc_fallbacks_total`: pages sent with registered
  `WRITE_FIXED` because kTLS rejected SEND_ZC; report the delta and rate.
- `unbounded_storage_tcp_rpc_lane_waits_total`: cumulative lane contention.
- `unbounded_storage_tcp_rpc_active_connections` and
  `unbounded_storage_tcp_rpc_inflight_requests`: lane establishment and achieved
  server concurrency.
- `unbounded_storage_frontend_response_bytes_total{frontend="loadgen"}` and
  `unbounded_storage_frontend_requests_total{frontend="loadgen",method="GET",
  status="200"}`: loadgen progress cross-checks.
- `process_cpu_seconds_total`: daemon CPU cross-check when `perf stat` is not
  being sampled.

Let `R_B0` and `R_B1` be Host B's received-payload counter at the two boundaries
and `T_B = t1_B - t0_B` seconds. For CPU, use each host's local interval `T_h`.
Compute:

```text
aggregate_useful_bytes = R_B1 - R_B0
aggregate_useful_Gbps = 8 * aggregate_useful_bytes / T_B / 1e9
daemon_CPU_cores_h = (process_cpu_seconds_h1 - process_cpu_seconds_h0) / T_h
TcpRetransSegs_h = TcpRetransSegs_h1 - TcpRetransSegs_h0
```

Read `TcpRetransSegs` from each `nstat` snapshot and subtract the numeric values;
do not use the reset semantics of plain `nstat` during the measurement.

Host B's receive counter counts each useful payload byte once across all Host A
shards, lanes, and concurrent RPCs. Cross-check it against Host A's sent delta;
requests cut by the exact boundaries may cause a small difference. Also report
NIC byte/packet/drop/error deltas from `ethtool -S <iface>` and TCP retransmit
deltas from `nstat`; there is no fixed retransmit rejection threshold in this
acceptance contract, but unexplained
loss or a one-sided result must be investigated and rerun.

Pass the transport only if at least one lane point reaches
`aggregate_useful_Gbps >= 95.0` for the complete 60-second window, Host A made
successful loadgen progress, Host B completed successful TCP RPCs, and
auth/protocol errors did not increase.
Connection errors during the measured window invalidate that point. Preserve the
full lane sweep, CPU data, retransmits, NIC counters, configs, binary revision,
and daemon logs with the result.

## Behavior Checks Outside Throughput

The throughput window exercises persistent connections and should cross the
65,536-request server rotation boundary. Confirm active connections recover and
payload continues without a permanent drop. Each connection carries one active
request, then is retired after 65,536 requests. The client discards the lane
when it observes the close, and a subsequent request establishes a fresh
OpenSSL handshake. Record any boundary request failure separately from the
sustained useful-payload rate.

Cancellation and disconnect are correctness checks, not throughput numerator.
Dropping a client stream queues best-effort `CANCEL` and closes the lane; a
disconnect fails the active request and releases server relay state. The next
request reconnects. The configured `request_timeout_ms` currently defaults to
30,000 ms but is not yet enforced by the TCP RPC driver, so do not report a
deadline-behavior pass from this run. Recursive forwarding starts at TTL 64,
decrements on each forward, permits a local owner at TTL 0, and returns error 508
instead of forwarding at TTL 0.
