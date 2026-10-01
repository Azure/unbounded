# Receiver RX checksum A/B/A helper

`hack/scripts/racer-rx-checksum-canary.py` is a separate operational experiment.
It does not change the TSO helper, application credits, load, or integrity checks.
Parent owns C8, authorization, fleet safety, stable images and exclusive settings
ownership. No cluster execution was performed during implementation.

## Scope and interpretation

### Reviewed profiles

The default remains `historical-ddv5-eg-to-adsv5-w`, preserving the original
pair below. Explicit `--profile ddv5-6o-to-adsv5-7n-20261001` selects the
October 1 recurring pair: remote `aks-ddv5-17198779-vmss00006o`, UID
`024467ab-e873-46d1-9d92-d15119f3ea28`, to receiver
`aks-adsv5-13731677-vmss00007n`, UID `4c4c2810-8c4d-49aa-8c2c-389f9e1728a4`,
actual dataplane pod IP `10.224.5.52`, access pod `unbounded-net-node-km9tz`,
and VF `enP1216s1`. Both ends were observed using mlx5, not MANA. The receiver
recorded 17 distinct acquisition/attempt identities from that remote between
14:26:05 and 14:41:00 UTC, including two new events during a 71-second read-only
observation. This identifies the last reverse signer, not the corruption origin.

Profiles are a closed allowlist, not arbitrary node/interface configuration.
Preflight checks receiver node name, UID, InternalIP, pod node placement,
readiness, actual dataplane IP, and the named host-network access pod/container.
Changed-stage launch rechecks these API identities. Remote entry checks hostname,
eth0 IP, hv_netvsc/mlx5 drivers, and the selected VF's eth0 lower-device association.
Authorization must match the selected receiver UID and `rx:on->off->on` action.
All existing timing, recurrence, feature comparison, and restoration gates remain.

Parent-only invocation for this profile (not executed during implementation):

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -u -B hack/scripts/racer-rx-checksum-canary.py \
  --profile ddv5-6o-to-adsv5-7n-20261001 --concurrency 10 \
  --authorize '4c4c2810-8c4d-49aa-8c2c-389f9e1728a4:rx:on->off->on' \
  --checkpoint tmp/rx-canary-6o-7n-run.jsonl
```

Parent must confirm the applied concurrency and exclusive settings ownership.
At the observed .01667 rejects/s, a 90-second off stage expects only 1.5 rejects;
silence is inconclusive. Require fresh pair exposure and software-checksum use,
not merely node-wide progress. No live execution accompanies this change.

Fixed receiver: `aks-adsv5-13731677-vmss00000w`, `10.224.4.69`, node UID
`7de2d936-08ba-4676-9218-30eff4c83380`. Sender identity is
`8816d91d-e896-49bf-ba8a-da97ede93818`. Access is through existing privileged
`unbounded-net-node-z6z2d` in `unbounded-system`, context `joolshev-scale-test`,
using `nsenter -t 1 -m -n`. VF is `enP60223s1`, not MANA.

Baseline is read-only for 45 seconds. Without a newly observed pair acquisition
and verified-byte progress, it exits inconclusive without any setting write,
including rollback writes. Baseline ring gaps also prevent mutation. The off
stage is 90 seconds (below the 120-second maximum), followed by 45 seconds
restored. Checkpoints target 20 seconds. All ethtool features on eth0 and VF
are saved; both must start RX on. Only `ethtool -K eth0 rx off` is requested.
Both interfaces must read RX off with every other feature identical. Dependent
feature changes abort and RX is restored; if full feature equality is not
recovered, the command fails and parent must investigate, not automatically
toggle additional settings.

The AEAD ring provides rejection timestamps, not acquisition-start timestamps
(`cmd/racer-dataplane/src/telemetry/failures.rs:289-323`). Fresh means a new ring
sequence and acquisition/attempt identity first observed in that stage. Retained
bad pages first decrypted later cannot be conclusively excluded. Pair rejects
are diagnostic during off, not an automatic abort. Readiness loss, digest mismatch,
metric reset/missing required counters and feature drift abort. CPU, verified
bytes, pull errors, TCP checksum errors and VF checksum/traffic counters are
recorded; these denominators are node-wide, NOT proven pair-transfer exposure.
Safety counters and seen acquisition/attempt identities persist across all three
stages. A new pre-mutation snapshot checks changes since baseline. This also
catches digest mismatches occurring between the off stage and restored sampling.
Thus disappearance alone is never called a fix. Parent must establish comparable
fresh pair traffic, software-checksum use, and recurrence after restoration.
Checksum-preserving corruption and post-validation corruption remain possible.
This preserves the never-return-corrupt requirement in `/home/azureuser/design.md:39`.

## Parent invocation (not executed during development)

Use an existing worktree tmp directory and a fresh output/checkpoint name:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -u -B hack/scripts/racer-rx-checksum-canary.py \
  --concurrency 8 \
  --authorize '7de2d936-08ba-4676-9218-30eff4c83380:rx:on->off->on' \
  --checkpoint tmp/rx-canary-run.jsonl
```

Concurrency is the applied loadgen concurrency, not client/server credits.
No credentials, payloads, object keys or environment are collected. Diagnostic
page hashes are pseudonyms. Remote samples stream as JSON lines and are flushed
to the local checkpoint. Source travels as a Python `-c` argument, not an
asynchronous stdin pipe. Non-JSON stderr is retained only for exact allowlisted
diagnostics (hostname mismatch, kubectl exit status, missing required tools).
Other lines are counted but suppressed. Remote Python failures emit structured
error kind/status/errno and controlled RuntimeError messages; command argv,
source/config and arbitrary subprocess stderr are never stringified. Collection
failure preserves already-streamed evidence. An inconclusive baseline exits zero
with an explicit `inconclusive` event, not a success verdict.

## Cleanup and limits

Python finally restores and compares all features. The remote shell traps
EXIT/TERM/INT/HUP and restores RX, independently of Python. A separate remote
timer restores RX at 110 seconds even if polling blocks. The timer is a separate
Python process with no polling dependency or orphaned sleep child; the shell
terminates and reaps it before exit. The parent performs a new independent
exec/readback after the changed runner exits, BEFORE launching restored read-only
observation. On ambiguous failure it waits until 130 seconds from launch (with
20-second recovery checkpoints), beyond the remote 115-second timeout plus
10-second kill grace, before independent restoration. Mutation has a 20-second
launch expiry, rechecked after reporting immediately before the write. Source
startup delays or failed pre-mutation checks cannot extend that window.

Exact budgets: baseline 45s under remote 60s/local 65s TERM; changed 90s under
remote 115s/local 120s TERM; restored 45s under remote 60s/local 65s TERM. Each
timeout has 10s kill grace. A parent alarm at 210s stops collection to reserve
cleanup within the mandatory external 300s command timeout. Preflight uses two
5s commands. Restored observation is skipped with an explicit inconclusive event
if more than 145 seconds have elapsed before its launch; this prevents a slow
rollback consuming the alarm from opening a new observation window. Preflight uses two
5s commands. Independent restore uses a 7s exec followed by two 5s readbacks,
each with 10s kill grace (47s worst case). An early changed-stage disconnect
waits out the fixed 130s remote lifetime, not a new relative interval. Parent
and remote Python cleanup ignore repeated TERM/INT/HUP/ALRM while restoring;
shell cleanup ignores TERM/INT/HUP. Health stages never resume after failure.
The changed-stage shell also waits for its terminated Python child before its
last restoration write; the enclosing remote timeout bounds that wait.

Unreachable hosts or SIGKILL can defeat restoration;
no cleanup success is claimed if verification fails. Parent must keep an
independent recovery path and not launch overlapping experiments:

```sh
timeout --signal=TERM --kill-after=10s 20s \
  kubectl --context joolshev-scale-test -n unbounded-system \
  exec unbounded-net-node-z6z2d -c node -- nsenter -t 1 -m -n -- \
  timeout --signal=TERM --kill-after=10s 5s ethtool -K eth0 rx on
```

Verify `ethtool -k eth0` and `ethtool -k enP60223s1` against the original saved
maps, plus readiness/progress. Stop the runner before emergency recovery; do not
leave an unsupervised remote operation. No automatic promotion or next experiment.

Offline tests (all host commands mocked):

```sh
timeout --signal=TERM --kill-after=10s 30s python3 -B hack/scripts/racer-rx-checksum-canary_test.py
```

Hostname comparisons normalize ASCII case in both entry and independent restore.
The receiver's actual host name ends in uppercase `W`, while its Kubernetes node
name ends in lowercase `w`. The first parent run on October 1 failed at this
literal comparison with shell exit 41 before Python baseline startup; both RX
features were subsequently observed on. Tests execute the real baseline entry
through a real shell with this uppercase hostname and mocked host measurements.

Development validation uses scoped Python syntax/indentation checks. `make fmt`
is a Go-only target (Makefile:531-533); its earlier bounded invocation expired
inside gofumpt without changing tracked files. It was not repeated. Black was
not installed in this environment; no dependency was added for this helper.
