# Dynamic host guard: phase-one operational prototype

Status: operational source component ready for parent review, **not activated**.
No release, rollout, cluster write, or image build. Historical phase blockers below
are superseded by this final validation section; deployment gates remain explicit.

## Final validation and exact deployment gates

### Guard polling and bounded readiness proof

The authorized polling optimization supersedes the older fresh-API readiness
description and 5s poll target below. `server.py` polls on a 10s schedule with a
stable SHA-256-derived per-node phase. Initial bootstrap and every DP startup
still fetch directly from the API; startup still invokes locked `Host.tick`.
The unchanged launcher nonce/node request cannot select the readiness path.

Deadline-aware refresh also schedules before the conservative kernel expiry
`valid_until - 6`, advancing by a stable per-node 3-8s spread. An unchanged
near-expiry API authority retries on a bounded 2-4s delay (or the next normal
phase), not a past-deadline busy loop. Healthy authority retains the normal10s
phase and API cost. These are scheduling targets, not a guarantee against slow
operations: all original expiry and fail-closed checks remain unchanged.

Readiness sends the distinct `{"ready": node}` request on the same private root
UDS. The server revalidates its in-memory CM authority, source UID, pinned node
UID/IP, current boot, sequence/content/expiry, exact live rules and1511 peer set,
and a live local freshness entry under the host lock. Only a successful direct
API fetch plus kernel reconciliation populates that memory. API age is measured
from **before** the fetch, using both monotonic and wall clocks, and must be at
most15s before and after kernel checks. No durable proof is restored on restart.
Errors invalidate the cache and retain the existing admission-close/expiry path.

Readiness does not fetch the API, invoke tick, write a proof file, change an ipset,
or renew API observation time or authority expiry. `verified` records the current
kernel check; separate `sourceVerified` retains the API observation time. The
client authenticates root socket ownership/peer credentials and checks returned
identity/boot/expiry/age. Offline fleet verification now also requires
`sourceVerified`<=15s, so collect new proofs; old proof JSON without that field is
not accepted. Fleet still requires exact1500 on the supplied current generation.

A missed update can leave readiness on the prior observation for at most15s, not
instantaneous revocation. Fresh API reads of an unchanged authority cannot extend
its original60s validity. `Host.tick`, kernel NEW gate, authority publication,
1511 membership, DENY11, and startup authorization are unchanged. Kernel timeout
remains the backstop even if the server dies or stops polling. The nominal healthy
steady-state load is1500/10 =150 source GET/s, rather than1500/5 +1500/10 =450;
startup/bootstrap reads and publisher traffic are additional. This is arithmetic,
not a measured live rate or a guarantee under delays. Slow operations fail proof
age checks rather than extending freshness; no image build is needed.

Focused offline validation, 2026-09-30: one run of
`timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest -v
test_ready test_start test_adapters` passed25 tests. Coverage includes repeated
readiness without fetch/renewal,15s boundary and delayed checks, missed content
update, original authority expiry after a recent read, boot/UID/content/source
drift, kernel peer/timeout drift, API failure, empty restart cache, authenticated
readiness client, every-start fresh fetch/tick, and distributed10s schedule.
Scoped `make fmt` on the unchanged adjacent launcher passed with0 issues.
No broad-suite repeat, image build, live kernel test, or cluster mutation occurred.

Guard-only canary path, **not executed by this change**:

1. Render locally using the existing trusted policy and pinned images. Create a
   new uniquely named immutable program CM, such as
   `racer-stage47-program-polling-<fix-sha-prefix>`, with all six source keys.
   Do not patch/recreate the old immutable CM or apply the entire renderer bundle.
2. Under a separately authorized rollout, preserve the guard DS UID/RV and all
   settings except its `program` CM reference. Use a reviewed OnDelete canary
   rollout so only the selected guard Pod is replaced initially, never introduce
   a second guard writer on the node. Preserve policy/source UID, RBAC, resources,
   NET_RAW/NET_ADMIN/SYS_CHROOT, images, DP template/launcher, and publisher refs.
3. Check actual mounted sources, current boot/source/kernel proof and exact1511,
   stable readiness over multiple polls, source age<=15s, original expiry, and
   source GET rate. In a permitted isolated canary exercise missed updates/API
   failure and guard/DP restart: stale readiness must fail, expired NEW must close,
   and every DP start must require fresh API plus kernel verification. Unit tests
   do not establish these live behaviors. Do not interrupt shared fleet authority
   or application load without separate operational authorization.
4. Expand only after those checks; collect exact1500 fresh proofs against current
   Nodes/source using the updated fleet verifier before claiming fleet completion.
   Keep the previous CM for rollback via the same controlled Pod replacement.
   A rollback restores higher API load, not a remedy for publisher budget failures.

### Publisher inventory memory fix

The released watcher at `dfb028607a22ae1bde4c01c703640441dc067dfa`
accumulated full decoded Nodes and namespace Pods before `contract.snapshot`.
The adapter now projects each page before retaining it and releases the raw page
before fetching another. `contract.snapshot` and its validation are unchanged.

Retained fields are Node metadata name/UID/deletionTimestamp and address type/value;
Pod metadata name/UID/deletionTimestamp/namespace, owner-reference
controller/kind/apiVersion/name/UID, spec nodeName/hostNetwork, and status podIP/podIPs.
`podIPs` retains the exact value, including any extra keys that must be rejected.
No label selector is safe here: stale named DS owners and host DP on DENY11 must
still be detected in the complete inventory. Labels, annotations, managedFields,
Node images, containers, and readiness are not authority inputs.

Requests again use 500 items with a finite budget of 100 pages, preserving the
nominal 50,000-object capacity (`watcher.py:27-29`). The intermediate projected
publisher used 100 items/500 pages; the operator reported no OOM but a 24.110s
two-inventory collection, exceeding the unchanged 20s freshness budget. Restoring
500 avoids five times as many list requests without undoing projection or raw-page
release (`watcher.py:115-118`). The unchanged 16MiB response cap is a separate byte
bound, not a guarantee that every 500 objects fit. A large object/page still fails closed;
there is no truncation, retry bypass, or partial publication. RV consistency,
continuation-loop rejection, both DS UID rechecks, both complete observations,
20s freshness, 25s cycle alarm, UID/RV CAS, and narrow RBAC remain in force.

Historical offline memory-fix validation on 2026-09-30 (not repeated for pagination):

- `timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest -v
  test_inventory test_adapters test_contract`: 23 tests passed.
- `timeout --signal=TERM --kill-after=10s 120s python3 -B inventory_memory.py`:
  fresh-process comparison against the exact released source, 1500 Nodes and
  9000 namespace Pods, two full inventory passes. Synthetic Kubernetes-shaped
  independently decoded JSON includes managedFields, Node images and Pod env/spec.
  Each worker is capped at 1536MiB address space and 90 CPU seconds.

| Implementation | Peak RSS MiB | Elapsed seconds | List requests | Largest page bytes |
| --- | ---: | ---: | ---: | ---: |
| Released negative control | 974.97 | 14.399 | 42 | 10,694,323 |
| Page projection | 50.45 | 11.112 | 210 | 2,138,965 |

Both published content digest
`4b2afb2956716bc6cd47f25501dcd21b3faf3290510d092a9c3a75694c48f8bd`.
The fixture checks released RSS exceeds 512MiB, projected RSS stays below 256MiB,
and at least a threefold reduction. This is synthetic process RSS evidence, not
a cgroup OOM replay, cluster measurement, or universal memory bound. Fixture clocks
are fixed; real elapsed includes page generation. More API requests can exceed the
unchanged real freshness budget under latency/throttling and must fail closed.

Pagination follow-up, one offline run on 2026-09-30:

- `timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest -v
  test_inventory test_adapters`: 19 tests passed in 1.144s. New focused assertions
  require 42 list calls for two 1500-Node/9000-Pod passes (not 210), preserve the
  50,000-object capacity, accept a 16MiB response and reject 16MiB+1 while closing
  the connection. Existing projection/security, raw-page-release, RV/continuation,
  DS identity, second-pass drift, and freshness rejection tests also pass.
- Existing `inventory_memory.worker("projected")` executed **once**, under external
  `timeout --signal=TERM --kill-after=10s 120s`, retaining its 1536MiB address-space
  and 90 CPU-second limits. No released negative-control or full-suite repeat.
  The wrapper asserted RSS<=512MiB, 42 list calls, max page<=16MiB and the exact
  content digest above. Result: **118.125MiB peak RSS**, 14.996s synthetic elapsed,
  **42 list calls**, largest response **10,694,323 bytes** versus 16,777,216 cap
  (6,082,893 bytes headroom). Same 1511-source authority and digest.
- Scoped `make fmt` on the adjacent launcher passed with 0 issues and no Go changes.

This fixture supports 500-item pages for the modeled object sizes, not arbitrary
Kubernetes objects or a universal 512MiB memory guarantee. Retention remains the
projected inventory plus one bounded decoded raw page; the old full-inventory
retention has not returned. JSON expansion and larger metadata can consume more
RSS than wire bytes; oversize responses still fail closed (`watcher.py:95-97`).
The fixed fixture clock does not establish live freshness. The 20s relist budget,
25s alarm and 60s authority validity remain unchanged (`watcher.py:168,178,207`),
as do security, UID/RV CAS, RBAC and resource limits. Live timing must be verified
by the operator after the separately authorized refresh, not inferred here.

Publisher-only operational refresh, **not executed by this change**:

1. From the reviewed fix, use `render.py` with the existing trusted policy and
   existing pinned Python/carrier digests, as a local review-only bundle. Its
   `program` ConfigMap includes the Python sources (`render.py:23-28`); the watcher
   executes `/guard/watcher.py` from that volume (`render.py:58-67`). No image build
   is needed for this Python-only fix; the launcher and images are unchanged.
2. Create a **new uniquely named immutable program ConfigMap**, for example
   `racer-stage47-program-pagination-<fix-sha-prefix>`, using all six reviewed source
   keys. Do not patch or delete/recreate the old immutable CM. Do not blindly apply
   the renderer's full bundle or reuse its fixed program CM name for changed data.
3. Review a narrowly scoped change to only
   `Deployment/racer-stage47-source-owner`'s `program` volume ConfigMap name, using
   the current Deployment UID/RV as preconditions. Preserve source CM UID/state,
   policy, service account, RBAC, images, resource limits, and all other template
   settings. Roll both publisher replicas to that new reference. Leave the host
   guard DS, host-DP template, launcher, and existing program CM consumers untouched.
4. Verify actual publisher-mounted source matches the fix, no new OOM restarts,
   and a fresh exact1511 authority renews within the existing budgets. Failed or
   partial collections must not renew. Source authority alone is not permission
   for fleet activation: retain the independent fresh1500 proof and every-start
   gates below. Retain the previous immutable CM for diagnosis; rolling back to
   a prior 100-item projected program preserves the memory fix but restores the
   observed freshness failure; the original released program restores the memory
   defect. Neither is a demonstrated recovery path.

### Fresh-bootstrap crash recovery

`bootstrap.py` now writes a root-owned0600, single-link regular-file intent in the
locked root guard directory before creating either set. Unique exclusive temporary
files, file fsync, atomic rename, and directory fsync make the ownership/generation
record crash-atomic; symlink, nonroot, and permissive markers are refused. Intent
binds bootID, source UID, content sequence/digest, exact1511 sources, node identity,
and both immutable policy and generated rules. A heartbeat with unchanged content
can retry; changed content/policy/boot cannot adopt that intermediate state.

Only an owned CLOSED creation prefix is recoverable: absent sets, empty freshness
alone, or empty freshness with a peer set containing the exact ordered population
prefix. All present sets must retain exact type/options, and kernel reference
counts must be zero (including other tables and list:set references). Staging sets,
foreign members, nonempty freshness, or listeners block recovery before mutation.
Cleanup deletes peers before freshness, so interruption leaves another valid prefix.
No live set is flushed or open rule repaired. Fresh authority and listener absence
are checked under the lock and again immediately before the filter transaction.
Cooperating starts must use the existing launcher/lock contract; this is not a
defense against privileged processes racing an independent bind or firewall write.

After a successful transaction, the marker is removed durably. A crash between
COMMIT and removal is accepted only with no listeners, exact current peer set,
empty freshness, and the complete exact policy. Installation never opens freshness.
The fixed transaction uses iptables-restore tokens, not shell quoting: shell-quoted
`'!'` is rejected by the real restore parser.

Focused `test_bootstrap.py` exercises injected failures after each creation,
partial population, before transaction, after COMMIT, and during cleanup, plus
foreign/active/drifted state and unsafe ownership files. `kernel_bootstrap_test.py`
runs only in an explicitly authorized disposable private root netns. Real TCP
peer18082 and diagnostic19090 tests establish fresh authorized success, unauthorized
rejection, and expired NEW rejection with gate counters increasing while the old
jump remains at zero. Established authorized flows continue after freshness expiry
in this test: **expiry does not revoke established flows**. Existing policy still
governs them, and source removal can change their behavior. This does not validate
the application, a host reboot, or fleet deployment.

- Full DP revision `5e1a4554f8034fef676d5ca91315da80646e5b71` recipe inspected with
  git show: entrypoint `/usr/local/bin/racer-dataplane`, default image user65532.
  Three-character `5e1` was an invalid lookup, not missing full revision evidence.
  Parent's root DP setting must be preserved and verified in rendered/live Pod;
  launcher refuses nonroot. Cached ff7fd image config independently matches path
  and image UID, but is not proof of target5e1 runtime configuration.
- Scoped make fmt succeeds with `GOTOOLCHAIN=go1.26.6`, repository
  `/home/azureuser/code/unbounded/bin/golangci-lint` (actual2.11.4), **0 issues**.
  No requirement to install another linter remained. Go launcher tests pass.
- Disposable Docker namespace kernel test PASS: real iptables exact old/new policy,
  barrier insertion, ipset swap/removal and kernel timeout expiration. Existing
  kindest/node cache used; ipset installed only inside ephemeral container. No
  image built, no host package installed, no hostNetwork, all containers removed.
  Test does not assert established-peer application behavior or reboot success.
- `ready.py` queries the root UDS for bounded-age API authority plus an active
  kernel check, as described above. Renderer wires it as readiness, not a saved ack.
- `ready.py --fleet <json>` verifies1500 unique fresh node/boot/source-content proofs
  against supplied current Nodes and source CM. Parent supplies trusted freshly
  collected proofs; this verifier does not collect, mutate or pretend files are
  authenticated by themselves. Kernel proofs must be <=30s old and unexpired;
  their API observation must also be <=15s old.

Deployment gates (parent-owned, no implicit permission):

1. Build/publish carrier only after authorization, pin its digest and reviewed
   Python runtime digest. Confirm actual root DP setting and executable path.
2. Create empty source CM, capture UID, construct exact1500 trusted NodeUID/IP fixed
   policy and fixed observed monitors/DENY11. Install narrow RBAC, start watcher,
   require valid complete authoritative1511 before host installation.
3. Review legacy-transition authorization and existing guard coexistence. Old guard
   verifier expects top original jump, so it will become unready after added prefix;
   do not allow two independent writers. Parent controls staged guard handover and
   keeps application template untouched until all1500 fresh proofs pass.
4. Collect fresh active proofs and Nodes including bootID; run fleet verifier.
   Independently preserve DP images/args/normal placement/DENY11 and template hashes.
   Do not infer fleet completion from source-CM write or DS Ready count alone.
5. Apply only reviewed host-DP override with declared copy init and stable launcher
   command; preserve args and affinity, and coordinate removal of obsolete frozen
   init reference. Verify every actual host DP is wrapped. Podnet11 is not wrapped
   with host gate. Exercise controlled container restart/reboot before claiming
   those operational scenarios validated. No automatic rollout/cleanup helper.

Clock synchronization is required for validity bounds; partitions fail admission
closed on kernel expiry, not instantaneous total revocation. Node UID/IP replacement
outside fixed bootstrap map requires a separately reviewed policy change. Root is
trusted. These boundaries are not hidden by source tests or readiness.

## Phase-four final status: source prototype, deployment HARD BLOCKED

Concrete remaining implementation from phase three is supplied:

- Watcher/server default to continuous operation with bounded25s cycles; optional
  --seconds is for local bounded runs. No periodic240s exits/kubelet backoff loop.
  TERM raises SystemExit so Python finally blocks close sockets and host children.
- Authorized legacy transition verifies exact old ordered chain/top jump/single
  reference and current fresh authoritative1511 membership, creates or verifies an
  empty freshness set, then inserts ONLY the NEW barrier before the existing jump.
  No flush, broadening, old-IP union or automatic repair. Existing drift fails.
- `images/racer-guard/Containerfile` carries a CGO-disabled Go launcher in Python3.12
  slim with cp. It has NOT been built. Image workflow `.github/workflows/images.yaml:63-74`
  discovers the Containerfile without an allowlist; no workflow edits necessary.
- Operator `override/allowlist.go:61-67,82-91,107` allows command/mount/init/volume
  fields; `override/doc.go:70-79` requires explicit added-init names. Renderer emits
  `hostDPOverride` targeting only racer-dataplane with addInitContainers. It leaves
  args and affinity unchanged, retaining ordinary DP placement/DENY11 exclusions.
  `apply_test.go:679-702` asserts declared-init acceptance; no application code changed.
- `source-bootstrap.yaml` creates no authority, only empty mutable state. Capture
  actual UID, pin immutable policy, start restricted watcher, then require complete
  fresh source before any host bootstrap/transition. No automatic orchestration.
- Synthetic Go exec test verifies same PID, preserved argument and direct SIGTERM
  delivery after exec. It does not claim complete real-DP integration.

Hard gates, no more automatic implementation phases:

1. Kernel integration **not passed**. Authorized sudo unshare-net succeeded; host
   iptables is present but ipset executable is missing. No installation or host
   firewall mutation attempted. Obtain a permitted isolated test environment with
   ipset and validate swap, timeout, existing-flow semantics and legacy insertion.
2. Scoped make fmt ran gofumpt successfully, then installed golangci-lint failed
   reading Go export data version4 (supports<=2). Compatible lint toolchain required.
   Do not label this source lint-clean.
3. The supplied `5e1` is not a resolvable git object here. Current recipe confirms
   /usr/local/bin/racer-dataplane, but deployed immutable image config/rootfs and
   runtime UID0 must be verified before launcher activation. Default recipe UID is
   65532; root is a parent-supplied deployment fact, not established by this recipe.
4. Rendered objects are review-only, not an activation bundle: no readiness probe
   currently proves guard authority/kernel state to Kubernetes, no exact fleet
   completion gate, no automated coexistence/removal plan for old guard DS, and
   no original init-reference removal. Parent must not apply all objects blindly.
5. Root-owned UDS authenticates host root, not individual Pod identity. All DP
   starts rely on launcher placement; existing unwrapped containers remain outside
   this guarantee. No defense is claimed against root rewriting firewall/socket.
6. Clock-jump/skew and crash-at-every-step tests, real UDS launcher/server handshakes,
   and exact operator-rendered integration still need validation before release.

This phase stops at a hard-block checkpoint. No build, deploy, C0 operation or
live firewall test has occurred. Older phase sections are historical, superseded
by this status and the exact checkpoint.

## Phase-three delta (current implementation)

`launcher/main.go` is a Linux stdlib-only every-start launcher. Build with
`CGO_ENABLED=0 go build -trimpath -o <reviewed-output>/racer-guard-launch
./hack/scripts/racer-dynamic-guard/launcher`. No image was built. It accepts only
the fixed UDS and fixed `/usr/local/bin/racer-dataplane` executable, forwards args,
uses a random nonce, validates same-boot/node/fresh proof and root SO_PEERCRED,
then syscall.Exec replaces PID1 including all Go threads. Startup denial exits;
there is no permissive fallback or retry loop. Directory0700/socket0600 root owner
are required. Current tests validate proof fields, not full exec/signal integration.

`server.py` provides single-threaded root-credential UDS service plus periodic
reconciliation. Every startup request reads the source CM directly, validates immutable
sourceUID and bootstrap NodeUID/IP pins, and invokes locked kernel verification
before responding. UID0 is trusted per the approved root DP requirement, not a
unique Pod authentication mechanism. It rejects DENY11 independently. No extra
cryptographic secret is required to authenticate host root within this trust model.
`watcher.API` also accepts a sourceUID pin; watcher CLI requires it.

`bootstrap.py` installs an empty freshness set, exact1511 peers and fixed rules in
one filter transaction, only with no listeners and no prior guard objects. It is
idempotent for complete exact policy and rejects partial or legacy state. Server
invokes it on EVERY guard process start, not just init. Guard hostPath uses
DirectoryOrCreate; server checks root ownership/nonwritability then chmod0700.
Thus no host service edit is necessary for recreation of the `/run` directory.

`render.py` emits review-only immutable program/policy CMs, watcher Deployment,
guard DS with exact1500 metadata.name affinity, and a host-DP merge fragment with
carrier-copy init and read-only launcher/socket mounts. Mutable source content is
not included in pod templates or hashes. Requires digest-pinned Python and carrier
images plus trusted1500 policy input. No rendered bundle is permission to apply.

Minimal remaining deployment blockers:

1. Safe **legacy-to-new fixed-policy migration** is not implemented. Bootstrap
   deliberately refuses the current original jump lacking NEW-expiry prefix.
2. Existing source CM must be precreated with `data.state.json={}` and UID captured;
   watcher must publish complete authority before bootstrap. No automatic CM create.
3. Carrier artifact/image, exact deployed root DP executable/UID confirmation, and
   operator acceptance of launcher command/mount/init fragment are not proven.
4. Runtime240s supervisors currently exit and rely on kubelet restart: repeated
   sub10-minute exits can accumulate CrashLoopBackOff and break freshness. A
   long-lived process with individually bounded cycles is needed before deployment;
   do not treat current bounded CLIs as production continuous agents.
5. Kernel integration could not run: isolated unshare user/net probe failed at
   uid_map with Operation not permitted. Need permitted isolated environment, not
   fleet tests. Crash/clock-jump/UDS exec/signals and actual RBAC validation remain.
6. Full formatting/lint did not complete: make fmt45s timed out during gofumpt;
   focused launcher gofumpt and CGO_ENABLED=0 Go tests passed afterward. No tracked
   application files changed. No complete-release or deployment-safety claim.

Older sections below describe prior phase intent and are superseded by these deltas.

## Phase-two implementation delta (supersedes missing-adapter notes below)

`watcher.py` now supplies stdlib HTTPS, per-request projected token/CA reload,
restricted paths, source-only UID/RV CAS leadership, bounded pagination, two full
identity-consistent inventory observations per publish, and a bounded polling CLI.
No watch is needed: full relist avoids watch-cache gap recovery and runs only on
the leader. Both list observations must fit20s, whole cycle25s, then sleep10s.
This is stricter than a single list but still not an atomic cross-kind snapshot.
`authority.contentDigest` excludes sequence/timestamps; content sequence increments
only on inventory changes. Every successful fresh relist updates validity from
collection start; failed relists/acquisition never renew authority. Old prototype
`source_cas` is not the writer path; `watcher.state_patch` writes the envelope.

`local.py` supplies bounded host-command adapters, root-owned bounded flock,
exact chain/set verification, staged atomic swap, local NodeUID/IP checks, atomic
proof-file replacement, and bounded polling CLI. It uses Python in the guard image
and chroots **child commands only** into `/host` for existing host ipset/iptables/
timeout tools. API/token access remains in the guard container. Required host state
directory must be root-owned, mounted at `/run/racer-guard`; no host service edits.
`rbac.yaml` supplies only the two dedicated accounts and required get/list/patch
grants; no Secrets/workloads/exec/create/update/delete access. No manifest applied.

### Explicit fixed-policy migration prerequisite

Pure source-set swapping cannot reject only NEW connections on agent death while
preserving current fixed rules. Phase two therefore requires a **reviewed additional
fixed INPUT rule before the original jump**, not a silent rule rewrite:
`local.admission_rule` rejects NEW TCP18082/19090 when destination is absent from
`R47_FRESH`, `hash:ip family inet maxelem1 timeout60`. Original RACER_STAGE47 chain,
R47_PEERS membership semantics, local Prom exception and single jump remain exact.
The adapter verifies this new two-rule prefix; it cannot run against the old fleet
without separately reviewed bootstrap. No adapter creates or changes iptables.
Fresh entry timeout is at most remaining authority validity minus6s (command budget
and rounding). Agent death leaves kernel expiry active. Failures attempt immediate
empty admission set; failed commands leave expiry as backstop. Stale NEW flows are
blocked, not all traffic instantly revoked. Existing flows still face the original
chain; removal of an old IP can affect those flows too. No new ESTABLISHED bypass
or claim of preserving every existing connection is introduced.

Freshness assumes bounded wall-clock skew. Kernel expiry is monotonic after entry
installation, but renewal uses wall clock; clock-jump handling still needs tests.
Kernel/root integration tests, validation of killed-child cleanup, crash/lock tests,
bootstrap/admission migration, deployment pods, durable high-water recovery, and
launcher remain blockers. Proof persistence fsyncs the file and parent directory.
Fake tests are not live firewall evidence.

### Launcher plan refinement

Use a static Go binary copied by operations init into shared emptyDir; host DP
command execs it on every container start, then launcher execs the unchanged Rust
binary/args only after fresh authority verification. No DP image modification.
Use a root-owned host UDS directory, not loopback TCP. Verify path components,
socket owner/type and server SO_PEERCRED UID0; server checks caller credentials
against the pinned DP UID and fixed node policy, never caller-supplied claims alone.
Root on the host is trusted already: no extra cryptography authenticates an
untrusted root that can edit the firewall. Nonce/bootID/fresh-authority response
prevents replay by ordinary clients, not malicious root. Keep socket read-only
mount for DP, no writable parent directory. Never infer node/pod identity solely
from UID shared by other containers. DENY11 remains an independent hard gate.
No launcher/UDS server is implemented this phase.

## Verified baseline and scope

Read-only active-worktree evidence (paths relative to `tmp/racer-stage47-mixed`):

- `guard-refresh.py:58-71` puts sources in a new immutable CM and changes guard
  template references/hash. `:144-153` updates the host init reference separately.
- `guard-runtime.py:81-106` uses one kernel ipset swap but has no cross-process
  refresh lock. `:137-158` only bootstraps absent state when no listeners exist.
- `split-prep.py:49-68` installs an init container, not an every-process-start gate.
- `guard-plan.py:37-50` owns `RACER_STAGE47`, `R47_PEERS`, and the top INPUT jump.
  This source file lacks the local Prom exception, whereas `guard-refresh.py:32-35`
  explicitly requires it in the mounted CM program. Preserve the **mounted**
  policy, not an unpatched copy of the helper.

`images/racer-dataplane/Containerfile:5,33-61` uses Debian bookworm-slim, installs
libgcc-s1 and optionally RDMA libraries, runs as 65532, and execs the Rust binary.
Python is not explicitly installed. A shell is expected from Debian, but exact
deployed-image capabilities are NOT proven: image digest filesystem inspection
has not occurred. This phase does not choose a Python/shell entrypoint.
Read `/home/azureuser/design.md:17-21,39,54-59`: firewall membership is not a
replacement for application signing, encryption, integrity, or control-plane identity.

## Stable authority

Precreate immutable `racer-stage47-program-v1` and `racer-stage47-policy-v1` CMs.
Policy freezes DENY11 names, namespace, two DS names, observed monitor addresses,
chain/rule contract and local exception `10.240.0.104 -> 10.224.0.5:19090` only.
Policy must be compared against the actual mounted policy on all nodes before
activation. Current Node IP changes can change destination-specific rules: this
prototype refuses local identity/rule drift; it does not rewrite those chains.

Precreate separate mutable `racer-stage47-sources`; one `state.json` holds lease
and one complete generation. Digest covers schema, monotonic sequence, freshness,
both current DS UIDs, 1500 Node name/UID/IP records, and 11 Pod name/UID/node/nodeUID/
ownerUID/IP records. No CIDRs, overlaps, partial generations or old+new bridge union.
Source data must never enter DP/guard template annotations or operator hash inputs.
CM names in both templates remain constant; poll via API, not delayed subPath mounts.

Dedicated watcher service account RBAC plan:

- ClusterRole Nodes get/list/watch (RBAC cannot constrain list to fleet identities).
- Namespace Role Pods get/list/watch; filter in code, not a security claim about labels.
- Namespace Role apps/daemonsets get only, resourceNames exactly the two DS names.
- Namespace Role ConfigMaps get/patch only, resourceNames only `racer-stage47-sources`.
- No create/delete/update, Secrets, workload writes, exec, or arbitrary host access.
  Bootstrap CM creation and fixed policy changes belong to a separately authorized operator.
- Guard SA gets only the named source CM. Its host capabilities/mounts remain a
  separate privileged boundary, never granted to the watcher.

Lease acquisition/renewal is UID/RV CAS in the same CM (no extra Lease RBAC).
Proposed lease 30s, renew 10s, full relist 10s; each API request <=5s, complete
relist <=20s. Watch is optional acceleration, never authority by itself. Expired
watch/RV410/page failure discards cache and relists. Recheck both DS UIDs after
lists; any DS change restarts snapshot. Kubernetes offers no cross-kind atomic
snapshot: rechecking narrows races, not eliminates them. Successful source CAS
fences competing publishers but cannot make API observation instantaneous.
These adapters/loops and bounded lease takeover are **not implemented yet**.

## Local refresh and proof interface

All bootstrap, refresh, gate and proof adapters must share one host-wide bounded
flock separate from xtables.lock. Stage exact1511 `hash:ip family inet maxelem1511`
in a private set, verify it, swap once with `R47_PEERS`, verify unchanged ordered
chain/top INPUT/single reference plus exact membership, then delete staging old set.
No live flush/add, additive repair, permissive fallback, or old-IP retention after
successful replacement. Crash after swap is idempotently reverified before proof.
Persist monotonic sequence/digest and bootID atomically; no proof on partial failure.
The prototype supplies this adapter interface, not a real kernel adapter or lock.

Fresh per-node proof carries Node UID/IP, bootID, generation sequence/digest,
verified time, expiry and exact1511 count. Readiness must actively verify policy
and source freshness, not merely read a durable success file. Fleet proof requires
all1500 exact nodes on one current generation plus unchanged DP template hashes,
placement/DENY11 and local Prom restriction. A proof from a previous boot is invalid.

Poll target 5s; proposed healthy budget is relist scheduling10 + acquisition20 +
CAS5 + guard poll5 + reconcile20 =60s from an observed change, not instant revocation.
This is a target, **not a guaranteed bound** under API outage, scheduler delays,
partitions or incomplete11 inventory. Record observed/published/applied timestamps
and report actual max lag. Expiry currently rejects refresh/start proof, but does
not revoke an already-installed old set: a real stale-authority quarantine policy
and active process behavior need explicit implementation/tests before deployment.

## Every-start integration gate

Do not claim init reruns on app restart or host reboot. Do not use postStart,
readiness, or a persistent ready file as listener admission. Do not edit host services.

Preferred next prototype: tiny static launcher injected by an operations init into
emptyDir, executed as DP command on **every container start**, retaining the original
binary/args via exec. It needs no Python/shell in DP. Connect to a root-owned local
guard UDS, present nonce + node/Pod identity, and require a fresh same-boot response
after direct source read and locked kernel verification; reject DENY11 regardless
of placement and preserve the normal host DP selector exclusion independently.
Keep unprivileged DP capabilities unchanged. A nonce/boot check prevents replay but
does not prevent a later external firewall flush: continued supervision/revocation
requires a separate policy and tests. On reboot the launcher waits/fails until guard
bootstrap completes; existing boot init alone is insufficient.

Before choosing or shipping: inspect pinned deployed DP OCI config/rootfs read-only,
confirm binary path/UID/architecture/mount execute policy, operator command override
support, and test crash/restart/reboot ordering. No launcher, UDS server, privileged
adapter or deployment manifest is supplied in this first phase. These are explicit
next artifacts, not implied protection for the current fleet.
