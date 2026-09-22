# Racer dataplane DST campaigns

Run from the repository root:

```sh
python3 dst/run.py run --profile pr --artifacts dst/artifacts/pr
python3 dst/run.py run --scenario simulator-contracts --seed 19
python3 dst/run.py report dst/artifacts/pr
timeout --signal=KILL 10s python3 -m unittest discover -s dst
```

The runner builds the attached library tests once with two compiler jobs, executes
one suite at a time with one test thread, and applies per-build/per-suite host
deadlines. A verified cgroup-v2 ancestor must cap memory at 23,000,000,000 bytes
and disable swap. Otherwise the runner enters a user systemd scope with those
limits. This bounds the aggregate process tree below 24 GB, including subprocesses.

`scenarios/baseline.json` owns the initial selector, owner, tier, and execution
contract inventory. Each run records all discovered library selectors, including
ignored tests as opt-in, and validates required regression selectors. Hybrid
negotiation tests remain in the native tier. The native tier requests io_uring;
this is not yet a provider, envtest, binary-test, or deployment campaign.

Artifacts include the resolved manifest, build identity and log, tracked dirty
patch and untracked-file list, per-suite logs, inventory, and incremental results.
The baseline wrapper records seeds but does not claim exact replay of arbitrary
legacy or hybrid fixtures. Its result counts are suite test executions, not unique
coverage cells: generated tests also occur in owning suites. Product assertion
failures are currently coarse libtest failures; the wrapper does not infer an
oracle ID from a panic string. A killed runner leaves `complete: false`.

## Verified baseline

At `cc0a20cd`, all five PR groups passed under the memory cap and deadlines:
15 simulator contracts, generated lifecycles, managed cluster, targeted cluster,
and causal routing (63 test executions total). The full-page latency test is a
required HTTP/RDMA regression, superseding the old ignored-503 description.
Confirmation/replacement regressions are explicitly required in the inventory;
their native execution is separate from these managed-group results.

The scope is dataplane infrastructure. The production Subscriber/controller
decision-core extraction and cross-language stepped bridge are deferred by user
direction. Legacy fixtures, scale suites, and native requirements must remain
visible while managed artifacts are introduced incrementally.
