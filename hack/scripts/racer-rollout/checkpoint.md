# Rollout implementation checkpoint

2026-09-30T06:49:00Z: before local implementation via apply_patch.
Last successful state: read-only live inventory and d738fa9f code inspected;
operator running, source owner has two replicas, local writer three pods.
No live mutations. Error: one unquoted zsh custom-columns query failed, corrected.
Next: add bounded phase script, exact firewall cleanup and transformation tests.
Implementation phase deadline: five minutes; heartbeat: each command / one minute;
success: runnable scripts plus passing focused tests. Parent executes cluster phases.

2026-09-30T06:52:30Z: implementation complete. Last successful commands:

* `timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py' -v`: 8 passed.
* Read-only `kubectl get cm unbounded-component-overrides -o json` piped to the
  transformation: passed, idempotent, net/machina documents byte-preserved.
* `timeout --signal=TERM --kill-after=10s 15s git diff --check`: passed.
* `timeout --signal=TERM --kill-after=10s 60s make fmt GO_PACKAGE_DIRS=./hack/scripts/racer-dynamic-guard/launcher GO_PACKAGE_PATTERNS=./hack/scripts/racer-dynamic-guard/launcher`:
  gofumpt passed, lint failed with installed toolchain export-data incompatibility
  (version 4 exceeds supported version 2). No Go changes. Not retried unchanged.

Next mutating phase: stage only hack/scripts/racer-rollout and commit
`fix(ops): add plain Racer rollout and firewall cleanup`.
Deadline: five minutes; heartbeat: command before/after; success: one commit,
clean assigned branch. No live cluster writes, no image builds, no load changes.
Parent retains the existing worktree for integration and executes README phases.

2026-09-30T06:54:14Z: before follow-up apply_patch. Parent reports operator scaled
to zero, with only old terminal pods left; previous wait timed out on those pods.
Next: ignore terminal operator pods and defer controller image change until the
new operator installs its wiring. Deadline three minutes, heartbeat each command,
success focused tests and a new commit. No cluster writes by this worker.

2026-09-30T06:55:00Z: follow-up implemented; all 13 tests passed via
`timeout --signal=TERM --kill-after=10s 45s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py' -v`.
`timeout --signal=TERM --kill-after=10s 15s git diff --check` passed.
Required scoped make fmt (45s bound, same launcher scope recorded above) ran
gofumpt; lint failed on the same Go export-data v4/v2 incompatibility. No Go edits.
Next mutation: stage the four changed rollout files and create a new commit with
`timeout --signal=TERM --kill-after=10s 60s git commit -m "fix(ops): ignore terminal pods and defer controller rollout"`.
No cluster commands executed. Parent can continue writers with the fixed script;
completed operator scale-down need not be repeated.
