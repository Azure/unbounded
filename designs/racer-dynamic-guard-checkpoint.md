# Dynamic guard checkpoint

## 2026-09-29T23:10:40Z - before prototype mutation

Worktree `tmp/racer-dynamic-guard`, branch `ops/racer-dynamic-guard`, base
`66616b3497d516fcb2f4745b814186a8ffb0dddf`. Phase deadline 23:15:10Z;
heartbeat 60 seconds. Success: offline authority/local-refresh prototype with
focused tests and explicit deployment gaps, or this checkpoint. No cluster writes,
image builds, application release changes, or edits to other worktrees.

Last success: read AGENTS.md, /home/azureuser/design.md, active operations helpers
read-only, and actual repository DP image recipe. Exact completed mutating command:
`timeout --signal=TERM --kill-after=10s 30s git worktree add -b ops/racer-dynamic-guard tmp/racer-dynamic-guard 66616b3497d516fcb2f4745b814186a8ffb0dddf`.
Current operation: apply_patch to add isolated Python prototype and tests under
`hack/scripts/racer-dynamic-guard/`. Next command:
`timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest discover -s hack/scripts/racer-dynamic-guard -v`.
Errors: none. No delegation tool is available; no subagents launched.

The deployed image filesystem is not yet inspected. Recipe alone establishes
Debian runtime and no explicit Python installation, not exact deployed capabilities.
Do not deploy an inferred shell/Python wrapper. Every-start integration remains
a separate gate. Parent retains C4/load and pending operator rollout ownership.

## 2026-09-29T23:14:00Z - after prototype, before final focused validation

Last success: six offline unit tests passed in 0.204s at 23:13:13Z; diff whitespace
check passed. `timeout --signal=TERM --kill-after=10s 15s docker image ls --filter
reference='*racer-dataplane*' --format '{{.Repository}}:{{.Tag}} {{.ID}}'` returned
only local `racer-dataplane:e2e ee07461f284f`, not the deployed pinned image. No
container started and no image pulled/built. Exact deployed rootfs remains unproven.

Current mutation: apply_patch adds stale named-DS Pod rejection and freshness
rechecks inside lock/after verification. Next exact command is the same bounded
focused unittest command above, justified by these new changes, then diff check.
No commands are running in the background. Errors: none. No Go files changed;
make fmt not run because no commit is being made in this incomplete phase.

Artifacts: `contract.py`, `test_contract.py`, `designs/racer-dynamic-guard.md`.
Contract and fake tests are complete as an offline proof-interface prototype;
operational implementation is NOT complete. Missing: API/relist/lease adapter,
real serialized ipset adapter, stale-authority action, manifests, exact pinned
DP rootfs inspection and every-start launcher/server. No claim of deployment safety.
Return checkpoint, not an incomplete operational commit. Leave this dedicated
worktree for continuation; do not cherry-pick or remove it while incomplete.

Next bounded phase: inspect exact pinned DP OCI config/rootfs read-only (not local
e2e), implement bounded host adapter with fake failure/crash/lock/expiry tests,
then leadership/relist and RBAC artifacts. Preserve hardcoded DENY11 and normal
DP exclusion independently; no shared manifest edits. Every-start integration
requires its own verified launcher design, not a Python assumption or init-only gate.

## 2026-09-29T23:14:05Z - phase returned

Final focused command completed successfully: six tests passed in 0.258s after
the final code changes. `git diff --check` returned success (files remain untracked,
so this is not a full patch-content review). `git status --short` shows only the
two new design/checkpoint files and isolated helper directory. No commit,
cherry-pick, worktree removal, cluster mutation, host firewall command, image build,
or active-worktree edit. Errors: none. Phase ends before 23:15:10Z deadline.
The next action is the bounded phase above, not a replay of completed tests or any
active operational command. Parent keeps ownership of safe load and operator fix.

## 2026-09-29T23:14:51Z - phase two before mutation

Deadline 23:19:51Z, heartbeat60. Resumed by inspecting worktree status and exact
checkpoint: only phase-one untracked artifacts, base unchanged, no running jobs.
Parent reports operator77aa Ready and unchanged C4; no cluster verification here.
Current operation: apply_patch for stdlib HTTPS/source-CM CAS watcher and host
adapters. Success: focused new adapter tests and reviewable artifact or explicit
incomplete checkpoint. Next test command (only new tests):
`timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest discover -s hack/scripts/racer-dynamic-guard -p test_adapters.py -v`.
No cluster writes, host commands, image builds, application changes or delegation.
Errors: none. Source digest will separate content from heartbeat validity.

## 2026-09-29T23:18:44Z - phase two after validation

Last successful command:
`timeout --signal=TERM --kill-after=10s 30s python3 -B -m unittest discover -s hack/scripts/racer-dynamic-guard -p test_adapters.py -v`.
11 new adapter tests PASS in0.400s; prior phase-one tests were not rerun.
Earlier new-test runs7 and9 passed; repeated only after new code/test additions.
Tests cover heartbeat/content separation, takeover/failed inventory, time budget,
content tampering, forbidden API access, pagination inconsistency, projected token/
CA reload, exact admission/chain prefix, set parser options, staging references,
swap ordering and expiry closure before peer mutation. All use fakes, no host or
API operations. No commands remain running. Errors: none.

Implemented artifacts: watcher.py (bounded executable HTTPS/relist/lease writer),
local.py (bounded executable host polling and real command adapter), rbac.yaml
(review-only grants), test_adapters.py. Existing contract gains content_digest;
phase-two design delta distinguishes implemented code from missing integration.
Source heartbeat keeps content sequence/digest stable and peer membership equality
avoids rewriting unchanged1511. Source validity advances only after two full valid
identity-equivalent observations. Cross-kind reads are NOT an atomic API snapshot.

Important policy boundary: local adapter requires a preinstalled `R47_FRESH`
kernel-timeout set and NEW-connection rejection rule before the unchanged original
jump. It does not install or modify iptables. This is a deliberate proposed fixed
policy extension needed for expiry after agent death, NOT compatible activation
against the current fleet without reviewed bootstrap. Expiry rejects new flows;
no claim of immediate total revocation or preservation of all established flows.
Membership removal still affects existing traffic under the original chain.

Remaining blocking work: failure/crash/clock-jump/lock-contention and child cleanup
tests; real isolated kernel integration; bootstrap with admission closed; state
CM UID pinning across delete/recreate and durable high-water recovery; complete
deployment pod manifests and explicit NODE_UID provisioning (not downward API);
static launcher/UDS server and pinned-DP-image inspection. No launcher chosen based
on an assumed Python/shell runtime; guard Python uses existing runtime per parent.
RBAC file has not been applied or API-server validated. No new dependencies.

Phase returns incomplete before23:19:51Z: no commit/cherry-pick/removal. Both phases
remain only in this dedicated worktree for coherent completion/review, not parent
integration. No make fmt (no commit, no Go changes). Next bounded phase should
address these correctness/integration blockers before adding an activation runner.
Parent retains unchanged C4, operator77aa and all cluster safety responsibility.

## 2026-09-29T23:19:42Z - phase three before mutation

Deadline23:24:42Z heartbeat60; inspected exact checkpoint/status, only isolated
untracked artifacts, no background jobs. Operation: apply_patch launcher/server,
identity pins and bootstrap. Success: bounded new tests and coherent commit if
complete, otherwise precise minimal gaps. No cluster/C0 operations or image builds.
Next tests: TERM/kill10 bounded Python test_start.py and Go launcher tests with
`-timeout=5m`. Errors:none. SourceUID and NodeUID map come from immutable bootstrap
policy, not downward metadata. No delegation tool available.

## 2026-09-29T23:23:24Z - phase three after focused validation

New artifacts launcher/main.go + main_test.go, server.py, bootstrap.py, render.py,
test_start.py. Watcher API/CLI gains sourceUID pin. Four Python startup/bootstrap/
manifest tests PASS0.240s; CGO_ENABLED=0 launcher Go test PASS0.003s. Exact commands:
`timeout --signal=TERM --kill-after=10s 25s python3 -B -m unittest discover -s hack/scripts/racer-dynamic-guard -p test_start.py -v`
and `timeout --signal=TERM --kill-after=10s 25s env CGO_ENABLED=0 go test -timeout=5m ./hack/scripts/racer-dynamic-guard/launcher`.
Prior Python phases were not rerun. Earlier3 startup tests/Go test passed before
renderer/reboot improvements; reruns justified by those changes.

Failures and recovery:
- One isolated command `timeout --signal=TERM --kill-after=10s 10s unshare --user
  --map-root-user --net sh -c 'command -v ipset; command -v iptables'` failed writing
  /proc/self/uid_map: Operation not permitted. No kernel writes or broader retry.
- `timeout --signal=TERM --kill-after=10s 45s make fmt` timed out during gofumpt
  (Makefile531), before golangci-lint. Inspected process list: no formatter child
  remained; git status only task untracked artifacts. No claim of deadlock.
  Narrow recovery: `timeout --signal=TERM --kill-after=10s 15s gofumpt -w
  hack/scripts/racer-dynamic-guard/launcher` succeeded. Full fmt/lint incomplete.

Last success: focused tests and status check. Current operation: persist this
checkpoint and phase-three design delta; no commands running. Return before
23:24:42Z. No cluster operations, image builds, host service changes, application
release edits, other worktree edits, commit/cherry-pick or worktree removal.

Precise minimal gaps: reviewed migration from current legacy fixed rules; continuous
bounded-cycle service instead of240s exits causing kubelet backoff; carrier image/
exact DP root capability and operator fragment acceptance; source-CM bootstrap UID
capture; isolated kernel/crash/clock/exec integration tests and completed fmt/lint.
Source creation and apply remain parent actions, never execute generated objects
blindly. Full list and implemented reboot behavior in design phase-three delta.
All phases remain uncommitted because deployment is not coherent/validated yet.

## 2026-09-29T23:24:47Z - phase four before mutation

Deadline23:29:47Z heartbeat60. Inspected unchanged task-only status. Authorized
sudo private netns probe succeeded in creating namespace; iptables present but
ipset executable absent (command exit1). No host network writes. Current operation
apply_patch continuous services, exact legacy transition, carrier and exec tests.
Next commands focused Python phase4 and Go tests with external TERM/kill10 bounds.
No image builds/deploy. Errors: kernel test prerequisite missing ipset; do not skip
or claim kernel validation. Image workflow63-74 accepts any images/*/Containerfile;
no new allowlist change needed. DP recipe ENTRYPOINT line61 fixed /usr/local/bin.

## 2026-09-29T23:27:20Z - phase four validation and hard-block decision

Completed: continuous watcher/server25s cycles; optional exact legacy insertion;
carrier Containerfile; source-bootstrap.yaml; operator addInitContainers declaration;
synthetic same-PID/args/SIGTERM exec test. No shared application/workflow edits.

Tests: new phase4 Python2 PASS; after shared verifier/bootstrap changes, all23
Python tests PASS1.007s. Exact command `timeout --signal=TERM --kill-after=10s 20s
python3 -B -m unittest discover -s hack/scripts/racer-dynamic-guard -p 'test_*.py' -v`.
`timeout --signal=TERM --kill-after=10s 25s env CGO_ENABLED=0 go test -timeout=5m
./hack/scripts/racer-dynamic-guard/launcher -run TestExecSignal -v` PASS0.008s.
Initial synthetic test failed5s readiness due duplicate environment key in its
test-only exec harness; os.Setenv replacement fixed it. No DP process was executed.

`timeout --signal=TERM --kill-after=10s 45s make fmt
GO_PACKAGE_DIRS=./hack/scripts/racer-dynamic-guard/launcher
GO_PACKAGE_PATTERNS=./hack/scripts/racer-dynamic-guard/launcher` completed gofumpt,
then failed golangci typecheck: Go export data v4 exceeds linter supportedv2.
No unbounded retry/toolchain install. Kernel probe is BLOCKED missing ipset, not
namespace permission this time. `git show 5e1:images/racer-dataplane/Containerfile`
failed invalid object name; exact deployed path/UID not established.

Current operation: persist hard-block report, inspect final diff/status. No jobs
running. No claim of release completion. Remaining gates are enumerated in design
phase-four status: kernel+lint environment, deployed image facts, actual readiness/
fleet/operator integration and old guard handover. Do not apply renderer output.
Return hard block rather than automatically adding another implementation phase.

## 2026-09-29T23:28:20Z - phase four final handoff

Final changed-function test run3 PASS0.123s including explicit TERM SystemExit.
Task-only17 files staged for precise diff review; cached whitespace check PASS.
No commit: critical kernel validation and compatible lint blocked, and renderer
has no proven readiness/fleet handover gate. Staging is not authorization to deploy.
All changes remain on ops/racer-dynamic-guard at base66616b34, original branch
untouched; no cherry-pick/worktree removal. No jobs remain. Deadline23:29:47Z met.
Exact final check commands: bounded git add of task paths, git diff --cached
--check, git status --short. No secrets staged; only source/tests/plans/RBAC/carrier.
User requested completion or hard block, so return this hard block, not another
open-ended phase. Parent keeps unchanged C4 and all live operational ownership.

## 2026-09-29T23:28:59Z - authorized resolution before mutation

Deadline23:33:59Z heartbeat60. Full5e1a4554f8034fef676d5ca91315da80646e5b71
Containerfile resolved: ENTRYPOINT /usr/local/bin/racer-dataplane, image UID65532.
Cached actual ff7fd image config independently same path/UID; not target5e1 evidence.
Repo bin/golangci-lint is2.11.4, works with GOTOOLCHAINgo1.26.6 and reports two real
errcheck issues; fixing them, not upgrading dependencies. Existing kind image has
Python+iptables but noipset. Next isolated ephemeral package setup/kernel test,
readiness/fleet proof implementation, focused tests, scoped fmt and commit review.
No fleet writes or image builds. Earlier5e1 lookup error was invalid3char abbrev.

## 2026-09-29T23:31:45Z - resolution after tests, before commit

Scoped fmt/lint PASS0 issues using GOTOOLCHAINgo1.26.6 + repo golangci2.11.4
(actual installed version, not2.13.1). Fixed real close/type-assertion errcheck
findings. CGO_ENABLED=0 launcher tests PASS0.008s. New readiness fleet test PASS.
Kernel PASS in ephemeral Docker private netns with NET_ADMIN: real legacy-prefix,
exact-chain, atomic swap old removal, freshness timeout. Command bounded75s outer,
60s inner, package install only ephemeral; no image build/host package install.
First preparation container removed; test container used --rm. No jobs remain.

Implemented active readiness and boot-bound current-generation exact1500 fleet
verifier, renderer readiness. Full target DP SHA recipe resolved; current root DP
override remains parent deployment fact. Final design top section supersedes old
blockers with exact deployment gates. Parent must collect proof inventory; no
claim that local tests prove live fleet. Next: focused readiness test after final
namespace/duplicate-node check, stage/review/check, one coherent source commit.
No parent operational worktree edits, no cluster writes, no release publication.

## 2026-09-29T23:32:30Z - final validation before source commit

All25 Python tests PASS1.090s after readiness/renderer changes. Scoped formatter/
linter0 issues and launcher Go tests remain valid (no subsequent Go mutation).
Actual isolated kernel test passed; both ephemeral containers removed. Staged
20 task files reviewed, cached whitespace check PASS, no secrets or app changes.
Next exact command: `timeout --signal=TERM --kill-after=10s 30s git commit -m
"feat(ops): add dynamic Racer authority and every-start guard"`.
No further build/deploy/test operation required for this source handoff. Parent
must perform explicit deployment gates from final design section; source completion
is not a claim of live1500 convergence or authorization to mutate the fleet.
