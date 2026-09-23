# Racer page striping and 64 MiB cutover

Metadata and payloads use their cache keys as routing keys. Metadata is
version-independent. A page key includes the namespace/object identity,
object version, object length, aligned offset, and page size. The first eight
digest bytes interpreted as a little-endian integer select the primary slot
modulo the slot count. Candidate `i` is `(primary + i) % slot_count`, bounded
by the configured owner-attempt limit. Topology epochs do not change value
identity. Independent hashes can collide; this is not round-robin placement.

Metadata resolves before pages. Every page gets a fresh route cursor, while
connection pools and owner health remain shared. A metadata or sibling page
retry never becomes another page's starting attempt. HTTP, RDMA admission,
and rejection attribution reconstruct the same canonical cache key.

## Breaking cutover

Routing algorithm 3 uses the RF06 cursor. Previous routing algorithms are
rejected. Deploy matching control plane, dataplane, and SDK versions together;
mixed-version operation is unsupported.

The RACERS05 slab format uses 64 MiB payload extents (16,384 underlying
4 KiB blocks). Earlier slab versions are rejected without modification.
Use a fresh cache path and allow an origin refill. Managed agents use
`/cache/cache-v5.slab`; operators can remove the previous cache file after
the cutover once they no longer need it. There is no in-place migration.

Capacity must be 64 MiB aligned and provide at least eight extents (512 MiB)
per shard. The standalone default is eight buffers per NUMA node and an
eight-worker cap. Explicit multi-NUMA configurations must provision 512 MiB
of locked page buffers per participating NUMA node, plus runtime headroom.
The managed one-shard configuration selects one NUMA node, eight buffers,
and a 2 GiB memlock limit within its 4 GiB memory limit. Page prefetch remains
bounded to two pages per request.

## Verification latency follow-up

TODO: Separate fast routing/storage regression checks from exhaustive Go
control-plane scale and historical-churn tests, and expose per-test progress
instead of buffering entire package results. Profile the slow cases under the
race detector before changing their coverage. Give CA-retirement policy tests
an injected clock while retaining a separately invoked real-certificate expiry
integration test. The latter currently waits for actual leaf expiry and skew.

For this cutover, the Rust all-target, doctest, and benchmark-contract suites,
SDK/dataplane interoperability, storage resize/restart, and live CA rotation
passed. The final broad Go race run was interrupted and its remaining slow
tests were skipped for this change; that run is not recorded as passing.
