# Project Signal recovery gate

This command preserves the canonical rev24 regression packet and implements the
rev25 recovery gate used before a Dapple certification result can become
eligible.

The authority observer and recovery verifier are separate operations:

- `observe` verifies an authority-signed freshness statement using the
  immutable trusted resolver configuration and advances durable current-latest
  state.
- `verify` opens that state read-only and rejects recovery evidence that does
  not exactly match the durable current-latest checkpoint.

ACK consumption, witness non-equivocation, and latest-checkpoint observations
use SQLite transactions with WAL and full synchronous durability. Production
deployment requires the database on durable storage with retention, backup,
and single-writer fencing. The authority root, ledger revision, and ledger
digest must be mounted read-only from separately accepted configuration. They
are never accepted from the recovery bundle.

The verifier does not issue live authority and does not perform the Node PATCH.
The caller may send or classify a Dapple PATCH only after:

1. a separately privileged observer has advanced current-latest state;
2. `verify` succeeds against the same durable database;
3. the native runner image, accepted baseline, target Node assignment, runtime
   policy, and one-use transfer authority have been explicitly approved.

Run the preserved rev24 suite and rev25 regressions:

```bash
PYTHONPATH=cmd/project-signal-recovery/reference/rev24 \
  python3 -m unittest discover \
  -s cmd/project-signal-recovery/reference/rev24/tests -v

PYTHONPATH=cmd/project-signal-recovery/reference/rev24:cmd/project-signal-recovery \
  python3 -m unittest discover \
  -s cmd/project-signal-recovery/tests -v
```
