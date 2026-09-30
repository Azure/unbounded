# Image-only Racer upgrade

`upgrade.py` is independent of `rollout.py`. It does not reinstall, stop/start
load, scale workloads, change rollout strategies, remove guards, create
identities, fetch Secrets, or perform infrastructure operations. Parent operators
own readiness, admission, compatibility, load, and rollback decisions. A new
binary can change reconciliation behavior; image-only API writes do not prove
that the new operator will preserve its generated resources. Review that release
and the live results before advancing phases.

Requirements: Python 3.9+, PyYAML, GNU timeout, and kubectl. Run from the assigned
worktree. Every invocation requires `--context joolshev-scale-test` and a dedicated
`--state` directory inside that worktree. State files are private and untracked;
they include ConfigMap/workload configuration, so treat them as sensitive even
though the tool never requests Secret resources or kubeconfig contents. Do not
commit state or send it to external services.

## Read-only preparation

Use a new state directory for each plan. The tool refuses to overwrite artifacts.
Both preparation phases only issue cluster GETs. They read Deployments,
DaemonSets, and ConfigMaps in `unbounded-system` and `racer-loadgen`; they do not
list pods or retrieve their mounted credentials.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/upgrade.py snapshot --context joolshev-scale-test --state tmp/image-upgrade-3235e717
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/upgrade.py plan --context joolshev-scale-test --state tmp/image-upgrade-3235e717 --sha 3235e717a8e11a625600f13e2dd542c07b903f0e
```

`--sha` selects all five `ghcr.io/azure/<image>:<full-40-character-SHA>` refs,
matching the manual image workflow. It does not verify publication or registry
immutability. Parent must verify the builds first. Prefer digests when available:
replace `--sha` with all five `--operator-image`, `--controller-image`,
`--dataplane-image`, `--gantry-image`, and `--loadgen-image` flags, each supplying
its exact repository and `@sha256:<64 lowercase hex characters>`. Individual
flags may also override selected roles alongside `--sha`. Short SHAs, mutable
names such as `latest`, and a different role's repository fail closed. No old
rollout digests are silently used as defaults.

`snapshot.json` is the initial non-secret resource backup. `plan` re-reads live
state and writes `plan.json` (before/after replacement bodies and review-time
inventory), `plan.diff`, and the SHA-256 approval token. Review all three outputs.
`plan` is the client-side dry-run; it performs no server dry-run write requests.

The required override targets are Racer's Deployment (container `controller`),
both named Racer DaemonSets (container `dataplane`), and Gantry's DaemonSet
(container `gantry`). Missing, duplicate, unfamiliar targets, missing container
images, duplicate YAML keys, or aliases sharing an edited image or its containing
structure fail closed. Unrelated anchors and aliases (such as shared volumes or
environment variables) are preserved. Only image scalar spans are rewritten;
surrounding YAML and comments are retained, and the result is checked semantically
to prove that no other fields changed.
The live Racer and Gantry workload layouts are validated but not directly
replaced: the operator reconciles those workloads from the image overrides.
The operator Deployment's `controller` container is upgraded directly.
Loadgen images are discovered by exact repository in regular containers of
Deployments/DaemonSets in the two namespaces, including control workloads.
Their arguments, including `--catalog-images=512`, verification/concurrency
settings, and all other fields are left as found, not rebuilt from defaults.

## Explicit apply phases (parent only)

After review, substitute the printed hash for `REVIEWED_HASH`. Execute each phase
separately and inspect its results before proceeding; this is not an unattended
rollout recipe. Updating overrides can immediately trigger reconciliation.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/upgrade.py apply-overrides --context joolshev-scale-test --state tmp/image-upgrade-3235e717 --approved-plan REVIEWED_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/upgrade.py apply-operator --context joolshev-scale-test --state tmp/image-upgrade-3235e717 --approved-plan REVIEWED_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/upgrade.py apply-loadgen --context joolshev-scale-test --state tmp/image-upgrade-3235e717 --approved-plan REVIEWED_HASH
```

Each apply reproduces and validates the image-only transformation, reads all
targets for that phase, rejects UID/configuration drift, then server-dry-runs
the replacements before sending real PUTs. Unexpected admission changes in the
dry-run response fail closed as well. Admission policy can still change after
the dry-run, so the parent must inspect actual results. Only status, managedFields,
resourceVersion, and generation may drift since review. A fresh resourceVersion
from the validated GET is retained in each PUT, so concurrent changes fail
rather than being overwritten. All live metadata, including annotations and UID,
is retained; status is omitted from the main-resource PUT. Replacements are not
atomic across resources. There is no force, retry, delete, or automatic rollback.

The internal phase limit is 285 seconds, with each kubectl bounded to at most
40 seconds plus a 10-second SIGTERM grace period. `checkpoint.log` records
timestamped before/after commands and the last success; command completion is
the heartbeat. On failure or interruption, inspect live state and that checkpoint
before resuming. An exact already-applied configuration is skipped on rerun;
configuration drift requires a new state directory and reviewed plan. Never
replay a completed phase just to observe readiness. Observe rollout/readiness
separately using bounded commands under the explicit context. The tool neither
pauses nor resumes traffic and does not claim rollout convergence.

## Local tests

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py' -v
```
