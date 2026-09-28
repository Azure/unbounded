# Worker startup blocked by an unchanged-snapshot poll

## Observed failure

On 2026-09-28, `joolshev-scale-test` remained at 702/1500 ready dataplanes
with dataplane `8b00c43f` and controller `02cd457f`. The remaining processes
repeatedly exited with `DeadlineExceeded`. Controller deployment readiness was
1/3, consistent with leader-only serving, rather than a failed replica rollout.

Read-only diagnostics on `racer-dataplane-ddqfl`, node
`aks-ddsv6-84072342-vmss00004r`, established:

- At 05:18:07 UTC, initial enrollment completed in approximately 234 ms and
  worker enrollment in approximately 135 ms. The third TLS connection immediately
  received snapshot data. The identity file was freshly persisted, both worker
  slabs were open, and the client socket was prepared before the timeout.
- An authenticated request using that node's existing identity returned snapshot
  sequence 1851, 1500 members, 198186 bytes, HTTP 200 in 47 ms. The following
  unchanged-cursor request returned HTTP 204 after 30.002 seconds. Diagnostic
  private material stayed in memory; no identity or Secret was modified.
- A subsequent 10.262-second controller metrics window counted 1647 successful
  GETs, 181 successful POSTs, 5 successful PUTs, and no additional request errors:
  approximately 160.5 GET/s and 17.6 POST/s. These counters aggregate API requests;
  they are not endpoint-specific TokenReview latency measurements. Production
  enrollment performs a TokenReview for each issuance (`internal/racer/bootstrap.go:52`).
  Client-side default QPS throttling is disabled by controller-runtime configuration
  (`internal/racer/manager.go:116`). The observed successful enrollment exchanges
  include that API authentication and issuance work.

## Cause and fix

Worker startup has a fixed 30-second lifecycle deadline
(`cmd/racer-dataplane/src/runtime/worker.rs:641`). After re-enrollment it awaits
`ControlClient::progress` while preparing the first snapshot's local resources
(`cmd/racer-dataplane/src/app.rs:931`). When another worker has not finished cache
preparation, the publication remains pending. The next control turn can send a
long poll using that pending sequence (`cmd/racer-dataplane/src/control/client.rs:619`).
The same future retries local installation while the HTTP request remains pending
(`cmd/racer-dataplane/src/control/client.rs:435`).

Local installation can therefore finish without completing the control future.
Startup previously waited for the entire future before checking the committed
snapshot again. An unchanged controller publication legitimately waits 30 seconds
(`internal/racer/publications.go:288-296`), exhausting the already-running startup
deadline. Worker scheduling determines whether the next poll starts before local
preparation completes, explaining partial rather than universal readiness.

Startup now checks the committed snapshot after polling local preparation and
control progress. Once installed, it drops the pending control turn and continues
recovery. Explicit errors and the original deadline remain authoritative. The
regression holds the second worker until the controller receives an unchanged
long poll, then holds that response throughout startup. Before the fix both
workers return `DeadlineExceeded`; after the fix both become ready while the
response remains held.

## Deployment

Deploy the dataplane fix with the existing controller `02cd457f`. No API QPS,
TokenReview admission, authentication, identity, TLS, or request-timeout tuning is
needed for this cause. Preserve the existing API FQDN overrides, resource/slab
settings, and identity volumes. Increasing `RACER_REQUEST_TIMEOUT_MS` does not
change the separate fixed worker lifecycle deadline. Reducing worker count could
change the race but is not a correction.

This investigation did not build or deploy an image. Live readiness recovery must
be checked after the corrected dataplane image is deployed: all 1500 nodes ready,
no new startup timeout exits, then the previously blocked owner/manifest checks.
