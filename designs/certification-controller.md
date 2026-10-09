# Unbounded Certification Controller

**Status:** Engineering design review draft

**Scope:** Certification of newly provisioned, repaired, stale, or suspect
compute capacity before production admission.

## Decision summary

- A Certification Controller owns certification lifecycle, profile selection,
  participant selection, evidence policy, eligibility, and remediation requests.
- Argo Workflows owns execution of suite graphs, including retries, deadlines,
  synchronization, fan-out, cancellation, and cleanup sequencing.
- A certification suite is the stable execution boundary. A suite may contain
  one command or a coordinated diagnostic procedure with internal discovery,
  monitors, collectors, stages, cooldown, and evaluation.
- Versioned profiles compose suites into node, rack, fabric, environment, and
  workload gates.
- Execution state and hardware verdict are separate. Infrastructure
  interruption does not automatically become hardware failure.
- Every run produces a typed result, immutable evidence manifest, and correlated
  audit history.
- Existing certification behavior can enter the system through an exact
  compatibility suite. Additional gates remain separate until a new profile
  version explicitly makes them authoritative.

## Goals

The system must:

1. Keep capacity blocked until its required profile produces current evidence.
2. Bind certification history to durable infrastructure asset identity while
   projecting current state onto the active Kubernetes Node.
3. Support initial burn-in, post-repair verification, idle revalidation,
   continuous fault response, and authorized on-demand runs.
4. Support node-local and coordinated multi-node suites without forcing every
   diagnostic operation into a separate pod.
5. Produce reproducible results tied to immutable profile, configuration,
   runner image, participant, topology, and artifact identity.
6. Preserve one writer for eligibility and one requester for remediation.
7. Apply fleet and failure-domain concurrency limits to disruptive testing.
8. Allow compatibility behavior and new certification gates to coexist without
   silently changing each other's verdicts.

## Non-goals

- The controller does not implement diagnostic commands.
- Argo does not decide whether capacity is trustworthy.
- Process exit code alone is not the certification contract.
- Kubernetes Events and pod logs are not the durable evidence record.
- Continuous monitoring does not replace invasive certification.
- The first release does not require all suites to share identical tests or
  thresholds.

## Conceptual model

| Resource | Purpose |
|---|---|
| `CertificationProfile` | Versioned gate graph, suite references, thresholds, evidence requirements, retry policy, and admission policy |
| `CertificationSuite` | Stable logical procedure implemented by a private or public runner |
| `CertificationRun` | One execution for one scope, participant set, profile version, and trigger |
| `SuiteResult` | Typed execution state, verdict, measurements, observations, failure category, and evidence references |
| `EvidenceManifest` | Immutable index of inputs, results, artifacts, checksums, topology, observation coverage, and cleanup |
| `CertificationRecord` | Durable asset-keyed history of runs, faults, remediation, and recovery |

## Architecture

![Certification system architecture](images/certification-controller/system-architecture.png)

### Ownership

| Component | Owns | Must not own |
|---|---|---|
| Certification Controller | Profile resolution, lifecycle, participants, scheduling protection, policy evaluation, eligibility, evidence references, and remediation requests | Test command execution or success inference from pod exit alone |
| Argo Workflows | DAG execution, retries for infrastructure failures, deadlines, synchronization, fan-out, cancellation, and cleanup ordering | Trust, eligibility, or hardware policy |
| Runner catalog | Resolution of logical suites to digest-pinned images, templates, privileges, adapters, and schemas | Profile selection or cluster state mutation |
| Suite runner | Tool execution, suite-local state, parsing, monitoring, artifacts, cleanup, and `SuiteResult` publication | Direct writes to normalized eligibility |
| Evidence service | Immutable manifests and artifact retention | Verdict reinterpretation |
| Audit pipeline | Decision and mutation history correlated by run identity | Replacement of evidence or result payloads |
| Infrastructure adapters | Durable asset identity and execution of approved lifecycle operations | Certification verdict policy |

## End-to-end lifecycle

![Certification lifecycle](images/certification-controller/run-lifecycle.png)

1. The controller observes capacity that requires certification.
2. It binds the Kubernetes Node to durable asset identity and current repair
   generation.
3. It resolves exactly one immutable `CertificationProfile`.
4. It records blocked or testing eligibility before invasive execution.
5. It creates a `CertificationRun` and an Argo Workflow.
6. Argo executes the profile's suite graph with required placement and
   synchronization.
7. Each suite publishes a typed result and evidence manifest.
8. The controller evaluates required gates and evidence completeness.
9. It advances eligibility, leaves capacity blocked, or requests remediation.
10. A repair creates a new generation that requires new verification evidence.

No profile match or multiple profile matches fail closed. The controller does
not substitute a smaller suite merely because resources are unavailable.

## Suite execution contract

![Suite contract](images/certification-controller/suite-contract.png)

A suite is a complete gate-level procedure. It may internally implement:

- Discovery and preflight invariants.
- Long-running collectors and monitors.
- Sequential or parallel stress stages.
- Shared state and event processing.
- Cooldown observation.
- Deferred threshold evaluation.
- Coordinated cancellation and cleanup.

Argo treats the suite as one task unless the suite contract explicitly exposes
safe child tasks. This preserves procedures whose monitors, baselines, samples,
and evaluators span multiple internal operations.

### Required runner inputs

- `CertificationRun` UID.
- Immutable profile and suite version.
- Runner image digest.
- Participant and topology identity.
- Target Node placement.
- Deadline and cancellation channel.
- Configuration and secret references.
- Evidence destination.
- Audit correlation fields.

### Required runner outputs

```yaml
apiVersion: certification.unbounded.io/v1alpha1
kind: SuiteResult
spec:
  runUID: 01K...
  suite: node-burn-in-v1
  execution:
    state: Completed # Completed, Interrupted, Error
    attempt: 1
    startedAt: "2026-10-09T10:00:00Z"
    finishedAt: "2026-10-09T11:00:00Z"
  verdict:
    state: Pass # Pass, Suspect, Fail, Inconclusive
    category: Healthy
  evidence:
    manifestURI: object://certification-runs/01K.../manifest.json
    manifestDigest: sha256:...
  cleanup:
    state: Completed
```

Execution state answers whether the procedure ran correctly. Verdict answers
what the completed evidence says about the target.

## Profiles and gate graphs

![Profile resolution and gates](images/certification-controller/profile-gates.png)

A profile selects suites based on declared capabilities and certification
class. Different environments may select different profiles while using the
same controller and result contracts.

```yaml
apiVersion: certification.unbounded.io/v1alpha1
kind: CertificationProfile
metadata:
  name: accelerator-capacity-v1
spec:
  selector:
    matchLabels:
      certification.unbounded.io/class: accelerator
  gates:
    - name: node
      suiteRef: node-burn-in-v1
    - name: rack
      dependsOn: [node]
      suiteRef: rack-fabric-v1
    - name: environment
      dependsOn: [rack]
      suiteRef: environment-validation-v1
  admission:
    requiredGates: [node, rack, environment]
```

Profiles own:

- Suite composition and dependencies.
- Repetition and duration.
- Required measurements and thresholds.
- Evidence and observation coverage.
- Retry policy by failure class.
- Concurrency and failure-domain locks.
- Required, advisory, and shadow gates.
- Eligibility transitions.

Environment-specific behavior must not become hidden controller logic. New
hardware generations, topology classes, and policy revisions use new profile
versions.

## Scheduling and concurrency

The controller protects targets before execution and delegates workload
admission to the batch scheduling layer.

Recommended Node state:

- `certification.unbounded.io/eligibility=blocked|canary|production`
- `certification.unbounded.io/profile=<profile>`
- `certification.unbounded.io/generation=<generation>`
- `certification.unbounded.io/state=pending|testing|passed|failed|inconclusive`

Recommended taints:

- `certification.unbounded.io/blocked:NoSchedule`
- `certification.unbounded.io/testing:NoSchedule`
- `certification.unbounded.io/impaired:NoSchedule`

Concurrency policy may limit:

- Total disruptive runs.
- Runs per rack or fabric domain.
- Coordinated multi-node suites.
- High-bandwidth storage or network suites.
- Automated quarantine and remediation rate.

A run waits when a required lease is unavailable. It does not silently skip the
gate.

## Evidence and data flow

![Evidence and audit data flow](images/certification-controller/evidence-audit.png)

Each run writes an immutable prefix:

```text
certification-runs/
  <run-uid>/
    manifest.json
    run.json
    topology.json
    suites/
      <suite-name>/
        result.json
        measurements.json
        observations.json
        stdout.log
        stderr.log
        raw/
    cleanup/
      result.json
    checksums.txt
```

`manifest.json` records:

- Run, asset, Node, profile, suite, configuration, and runner identity.
- Participant and topology snapshots.
- Artifact paths and SHA-256 digests.
- Measurement and observation coverage.
- Result schema version.
- Cleanup status.
- Creation and retention policy.

Kubernetes stores current reconciliation state and evidence links. It is not
the high-volume or long-term artifact store.

## Auditing, logs, metrics, and traces

All records share the `CertificationRun` UID. Suite and task IDs provide child
correlation.

| Record | Purpose |
|---|---|
| Audit journal | Authoritative history of requests, decisions, state changes, publications, and remediation requests |
| Workflow and task logs | Execution debugging |
| Structured runner events | Progress, heartbeats, measurements, warnings, interruption, cleanup, and artifact publication |
| Metrics | Queue time, duration, outcomes, retries, failures, cleanup failures, and publication lag |
| Distributed traces | One-run path across controller, workflow engine, runners, stores, publishers, and lifecycle APIs |
| Evidence manifest | Reproducible inputs and outputs that anchor the verdict |

![Audit event sequence](images/certification-controller/audit-sequence.png)

Minimum audit sequence:

1. `RunRequested`
2. `ProfileResolved`
3. `EligibilityBlocked`
4. `WorkflowSubmitted`
5. `SuiteStarted`
6. `SuiteCompleted` or `SuiteInterrupted`
7. `EvidencePublished`
8. `VerdictRecorded`
9. `CompatibilityOutputPublished`, when configured
10. `EligibilityChanged` or `RemediationRequested`

Audit events include actor identity, timestamp, asset and Node identity,
profile/configuration/runner digests, workflow identity, evidence digest,
previous and new state, reason, and operation result.

Logs and metrics explain execution but cannot replace the typed result and
evidence record. Secrets, tokens, private implementation contents, and
unrestricted environment dumps must not enter logs or evidence.

## Compatibility mode

![Compatibility migration](images/certification-controller/compatibility-migration.png)

Compatibility mode brings an existing certification procedure into the new
control plane without translating every internal operation into a separate
workflow task.

The compatibility suite preserves:

- Participant and device selection.
- Runtime image, configuration, environment, mounts, and privileges.
- Discovery and preflight behavior.
- Stage ordering and concurrency.
- Monitors, collectors, shared state, and cooldown.
- Thresholds, rules, failure categories, and result mapping.
- Cancellation and cleanup behavior.
- Required operational outputs.

The adapter wraps the native result in `SuiteResult` without re-evaluating it.
Additional evidence is allowed. A different compatibility verdict is not.

New gates run separately as shadow or advisory gates. They become admission
requirements only through a new profile version.

### Migration phases

1. **Wrap:** execute the current-equivalent suite through the new run, evidence,
   and audit contracts while existing authority remains.
2. **Shadow:** compare targeting, environment, measurements, verdicts,
   categories, outputs, cancellation, and cleanup.
3. **Cut over:** make the Certification Controller the sole eligibility writer.
4. **Expand:** promote additional gates independently through versioned policy.

At every phase, one named component owns eligibility mutation and one named
component may request remediation.

## Failure semantics

| Situation | Execution state | Verdict | Default action |
|---|---|---|---|
| Procedure completes and required evidence passes | Completed | Pass | Advance the gate |
| Procedure completes with a hardware-policy violation | Completed | Fail | Keep blocked and evaluate remediation |
| Procedure completes with warning evidence | Completed | Suspect | Apply profile policy |
| Pod eviction or transient infrastructure loss | Interrupted | Inconclusive | Retry by infrastructure policy |
| Runner or parser defect | Error | Inconclusive | Keep blocked and alert the service owner |
| Missing evidence or cleanup proof | Error or Interrupted | Inconclusive | Keep blocked |

Hardware failure is not retried as infrastructure interruption. Infrastructure
retry does not erase earlier attempts; each attempt remains in the evidence and
audit history.

## Security and confidentiality

- Runners use digest-pinned images and least-privilege service accounts.
- Public resources reference logical suites rather than implementation contents.
- Secret references remain external to profiles and results.
- Artifact access is separate from operational log access.
- Runner-specific redaction policy applies before upload.
- Audit writes are append-only and idempotent.
- Retention classes distinguish audit records, raw logs, measurements, and
  large artifacts.
- Privileged infrastructure administrators can inspect running containers.
  Private images reduce distribution but do not provide absolute secrecy.

## Reconciliation outline

```text
reconcile(target):
  identity = resolveDurableIdentity(target)
  history = loadCertificationRecord(identity)
  profile = resolveExactlyOneProfile(target, history)

  if evidenceIsCurrentAndHealthy(history, profile):
      projectEligibility(target, history)
      return

  protectFromProductionScheduling(target)

  run = findOrCreateCertificationRun(identity, profile)
  workflow = ensureWorkflow(run)

  if workflowIsActive(workflow):
      updateProgressAndAudit(run, workflow)
      return

  result = loadAndValidateEvidence(run)

  if result.requiredGatesPassed:
      recordPassAndAdvanceEligibility(identity, result)
  elif result.retryableInfrastructureFailure:
      scheduleRetry(run, result)
  else:
      recordNonPassingResult(identity, result)
      requestRemediationWhenPolicyAllows(identity, result)
```

## Rollout and acceptance

The initial rollout should:

1. Deploy CRDs, controller, workflow templates, evidence storage, and audit
   pipeline without changing production eligibility.
2. Run compatibility suites in shadow mode on representative capacity and
   injected failures.
3. Compare participant selection, runtime environment, measurements, verdict,
   failure category, outputs, cancellation, and cleanup.
4. Validate dashboards, alerts, artifact retention, and audit reconstruction.
5. Exercise authority cutover and rollback.
6. Enable the controller as the sole eligibility writer.
7. Add new advisory gates and promote them through new profile versions.

Acceptance requires:

- Exact profile resolution or fail-closed behavior.
- Complete evidence and cleanup proof.
- Reproducible profile/configuration/runner identity.
- No dual eligibility or remediation writers.
- Explicit distinction between interruption and hardware failure.
- Successful audit reconstruction from request through final mutation.

## Open decisions

- Final API group, resource names, and schema versions.
- Evidence store and audit-store implementations.
- Default retention classes.
- Ownership of profile and runner-catalog publication.
- Whether compatibility outputs are controller-side or workflow-side
  publishers.
- Initial scheduler and failure-domain lock implementation.
- Policy for promotion from advisory to required gates.
