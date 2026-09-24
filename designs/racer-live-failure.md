# Resolved Racer live-campaign traffic failure

> Historical pre-stateless campaign evidence. The signed-claim certificate,
> revision-checkpoint, and expiry-overlap rotation changes require their own
> verification; this record does not establish that they passed. See the current
> [control-plane contract](racer-rust-controlplane.md).

## Final result

The production-binary campaign passed with **3,518 continuous verified SDK reads**
across CA generations 1 through 4, full old-root retirement, control-plane
failover, and storage resize/restart. The saved run is
`tmp/live/racer-live-run-7xgD7B.log:3-37`. This supersedes the failed development
runs below; no traffic errors are accepted or hidden by harness retries.

The campaign uses two Rust control planes, three production dataplanes, real
envtest Kubernetes, private network/mount namespaces, 120-second leaves and
one-second skew. See `e2e/racer-controlplane/README.md` for the maintained runner
and its prerequisites. The test is a correctness campaign, not a fleet-scale
latency or throughput measurement.

## Diagnosis retained for regression context

Earlier runs reached generation 4 but failed continuous reads with HTTP 502.
That distinction mattered: successful root retirement alone did not prove
uninterrupted traffic.

The captured reproduction in `tmp/live/racer-live-run-vStTeT.log` failed after
2,428 reads. Its persisted dataplane log recorded a reused peer TLS connection
failing in the response-header phase with `BrokenPipe`, rather than a downstream
HTTP error. The failure coincided with the remote peer's connection drain after
leaf renewal. An earlier capture in `tmp/live/racer-live-run-09dRzc.log` showed
all workers still healthy on generation 3 when the 502 occurred, before the
generation-4 trust projection.

The integrated peer keepalive retirement/recovery fixes resolved the failure.
Current regression coverage includes
`cmd/racer-dataplane/tests/http/peer_rotation.rs:5` and
`cmd/racer-dataplane/tests/http/peer_recovery.rs`; the full campaign then verified
traffic across actual expiry-gated CA retirement. Recovery remains within the
dataplane's bounded request execution rather than adding retries to the campaign.

The same campaign also exposed missing certificate SKI/AKI extensions under the
dataplane's strict OpenSSL verification. Rust security tests now cover the issued
chain with that verifier. The unrelated standby test failure was a fixture bug:
immutable ConfigMaps prohibit payload changes, but allow metadata updates used
for fencing. `cmd/racer-controlplane/tests/service.rs:279-290` now models that
contract and explicitly tests metadata updates and rejected payload changes.

## Diagnostics

The live runner preserves CP/DP logs, binary SHA-256 identities, timestamped
public trust/status/metrics observations, and full campaign output under
workspace-local `$TMPDIR`. Credentials remain in automatically cleaned test
directories. `hack/scripts/racer-live-observations.py` remains a small read-only
viewer for these artifacts: it reports TLS changes and changed error counters
without starting processes or rerunning a campaign.

The investigation is closed. See `designs/racer-go-retirement-audit.md` for final
verification status and the explicitly unverified 10,000-node first-byte tails.
