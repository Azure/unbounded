# Isolation modes test report

Run 20260101-000000: 1 passed, 1 failed, 1 skipped, 0 errors, of 3 scenarios. Started 2026-01-01T00:00:00Z, finished 2026-01-01T01:00:00Z.

## Environment

| Item | Value |
|---|---|
| git commit | 0123456789ab |
| git branch | impl/isolation-wp0 |
| host | runner |
| host base OS (VM) | ubuntu2404 |

## Summary

| ID | Scenario | Start mode | e2e suite | Result | Time |
|---|---|---|---|---|---|
| T01 | Every ordered pair of modes | nspawn (default, no flag) | isolation-matrix | PASS | 25m00s |
| X01 | none: all operations | none | isolation-operations | FAIL | 11m40s |
| G01 | GPU workloads in none mode | none | - | SKIP | 0s |

## Operation coverage by isolation mode

Each cell is the worst result of every step that exercised the operation while the node was in that mode. A dash means no scenario ran the operation in that mode.

| Operation | nspawn | none | container |
|---|---|---|---|
| Join (bootstrap) | PASS | PASS | - |
| Workload and DNS | PASS | PASS | - |
| NodeReboot (node stack restart) | - | FAIL | - |

## Mode transitions

In place is supported between nspawn and container only; any move involving none is a reprovision, as none has no slot to prepare the target in beside the source.

| Transition | inPlace | reprovision |
|---|---|---|
| none -> nspawn | - | PASS |
| nspawn -> none | - | PASS |

## Scenario details

### T01: Every ordered pair of modes (PASS)

| Field | Value |
|---|---|
| Suite | isolation-matrix |
| Start mode | nspawn (default, no flag) |
| Duration | 25m00s |
| Log | T01/suite.log |

Bootstrap with no flag, then change the mode through every ordered pair.

Expected: Each pair completes and each stop runs in the recorded mode.

| # | Step | Mode | Result | Time |
|---|---|---|---|---|
| 1 | run-agent | nspawn | PASS | 5m00s |
| 2 | validate-workload | nspawn | PASS | 40s |
| 3 | change-to-none | nspawn | PASS | 6m40s |
| 4 | validate-workload | none | PASS | 35s |
| 5 | change-to-nspawn | none | PASS | 7m00s |

Evidence captured after the suite:

```
{
  "machine": {
    "observed": "nspawn",
    "spec": ""
  }
}
```

### X01: none: all operations (FAIL)

| Field | Value |
|---|---|
| Suite | isolation-operations |
| Start mode | none |
| Duration | 11m40s |
| Log | X01/suite.log |

All operations in none mode.

Expected: Every operation passes.

| # | Step | Mode | Result | Time |
|---|---|---|---|---|
| 1 | run-agent | none | PASS | 5m00s |
| 2 | validate-isolation-soft-reboot | none | FAIL | 1m30s |
| 3 | validate-workload | none | not run | 0s |

Failure at step validate-isolation-soft-reboot: NodeReboot rebooted the host

```
[INFO]  [2/3] isolation-operations: validate-isolation-soft-reboot
[ERROR] NodeReboot rebooted the host; it must restart only the node stack
```

### G01: GPU workloads in none mode (SKIP)

| Field | Value |
|---|---|
| Suite | - |
| Start mode | none |
| Duration | 0s |
| Log | G01/suite.log |

NVIDIA wiring.

Expected: A GPU pod runs.

Note: Needs hardware.

Skipped: needs GPU passthrough hardware, which a plain QEMU VM does not have
