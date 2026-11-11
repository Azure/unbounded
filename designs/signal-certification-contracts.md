# Project Signal certification contracts

## Contract boundaries

Project Signal separates reusable test definitions, Suite scheduling, environment
bindings, immutable Run plans, durable invocation results, and qualification
policy.

- `CertificationTest` defines what one logical test executes and measures.
- `CertificationSuite` composes approved test identities into a dependency DAG.
  Stages in one phase may execute concurrently.
- `CertificationProfile` selects participants and binds environment resources.
- `CertificationRun` freezes the selected Suite, Profile, test definitions,
  mutable input resolutions, schedules, assertions, environment, and runtime
  provenance.
- Durable invocation envelopes contain complete measurements and identities.
  Run status contains bounded summaries and immutable evidence references.
- `CertificationPolicy` decides which trustworthy Run evidence grants
  qualification credit.

The API is a contract. Controllers, adapters, runners, resolvers, deployment
configuration, and admission controls must enforce it before qualification is
enabled.

## Test definitions and trust

`CertificationTest` is cluster-scoped. Its metadata name is the stable machine
name; its spec provides an operator display name, owner, version, typed
parameters, typed metrics, participant topology, a driver, and explicit
security requirements.

Participant roles are optional. Omitting `topology.roles` and explicitly
supplying an empty role list are equivalent role-free topologies; per-role
cardinality validation runs only when roles are present.

Supported drivers are:

- `Script`: a digest-verified inline `CertificationTest` artifact executed by
  a digest-pinned runner image.
- `Container`: a digest-pinned OCI image with bounded command, arguments, and
  resources.
- `WorkflowTemplate`: an approved template identified by a canonical SHA-256
  digest for coordinated multi-component tests.

The trust status is administrator-controlled:

- `Approved` test identities may grant qualification credit.
- `Diagnostic` test identities may execute only with
  `diagnosticOnly: true` and cannot grant qualification credit.
- `Rejected` and `Revoked` identities cannot be resolved into new Runs.

Approval applies to the canonical test content digest, not only the object
name. A later test generation requires a new approval decision.

## Suites and scheduling

New Suites use operator-named stages with explicit dependencies. Phases remain
as compatibility barriers for stages that omit `dependsOn`. Startup and Terminal
phases contain only `RunOnce` stages. Validation phases may contain `RunOnce`,
`Continuous`, or `Periodic` stages.

- `RunOnce` executes one invocation bounded by `invocationTimeout`.
- `Continuous` executes runner-owned bounded invocations without an intentional
  interval across `coverageWindow`.
- `Periodic` executes on an anchored `interval` across `coverageWindow`.

Continuous and Periodic stages declare a minimum execution count and maximum
start gap. Optional stages are reported but cannot hide required-stage
failure. Retries are bounded and apply only to explicitly classified
infrastructure failures. A retry never erases a workload failure. Derived
backend deadlines reserve every allowed `RunOnce` attempt and the bounded retry
tail after a repeated stage's coverage window.

Kubernetes object liveness and API polling timestamps are not workload
activity. Workload-produced samples and invocation records establish coverage.

## Profiles and immutable resolution

Profiles bind participant selectors and roles, namespace, runtime class,
storage, network, secret names, resolver identities, model caches, object
stores, and resource constraints. Secret values never enter Profile, Run,
plan-digest, or evidence objects.

Before a qualification Run starts, the controller and approved resolvers freeze
the complete plan:

- Test name, UID, generation, content digest, and trust state.
- Driver source, image, template, and runner digests.
- Typed parameter values and declarative assertions.
- Stage dependencies, compatibility phases, schedules, retries, and topology.
- Profile environment bindings and participant role assignment.
- Mutable authoring requests and their immutable resolutions.
- Controller, adapter, resolver, runner, and workflow provenance.

A mutable request such as `latest` is retained only as `requested`. Execution
uses the corresponding immutable `resolved` identity and SHA-256 digest.
Unresolved tags, aliases, ConfigMap content, templates, or model revisions are
ineligible for qualification.

The canonical `planDigest` covers every resolved execution-affecting field.
Kubernetes map and set lists are sorted by their declared key or value before
hashing, while phase and stage order remains significant. Equivalent resolved
plans have the same digest; any schedule, assertion, parameter, environment,
trust, artifact, image, model, template, or provenance change produces a
different digest.

## Results and verdicts

Every runner-owned invocation writes a versioned
`signal.unbounded-cloud.io/invocation-result/v1` envelope. It includes the Run,
stage, test, plan, participant, artifact, fingerprint, timing, outcome,
reason-code, measurement, device-coverage, and workload-activity identities.
The adapter hashes the canonical envelope and persists it immutably.

An assertion result normally records its typed actual value. When a required
metric could not be produced or observed, `actual` is absent, `passed` is
`false`, and `message` records the failure reason. The immutable Run plan still
contains the typed expected value or threshold. Assertion results are evidence
only and do not change `planDigest`.

Missing, malformed, oversized, replayed, partial, mismatched, untrusted, or
unverifiable evidence is `Inconclusive`. It is never converted into success.

Verdicts are layered:

1. Invocation verdict evaluates a valid result and invocation assertions.
2. Stage verdict evaluates required window, count, cadence, gap, assertion, and
   per-device interval coverage.
3. Run verdict evaluates required Startup, Validation, and Terminal stages.
4. Qualification policy evaluates trustworthy participant-scoped Run evidence.

Scripts may propose outcomes, but platform-owned typed assertions are
authoritative. The initial assertion vocabulary supports booleans, status,
minimum, maximum, range, count, error-rate, and percentile bounds without
embedding arbitrary controller-side scripts or CEL.

## Migration and fail-closed behavior

Existing Suite, Profile, Run, and evidence objects continue to decode.

- Legacy Suite `checks` and new `phases` are mutually exclusive.
- Legacy Run `checks` and new `resolvedPlan` are mutually exclusive.
- A legacy check without `execution` retains `RunOnce` behavior.
- Legacy `minDuration` measures observed backend lifetime only. It does not
  establish sustained workload activity and cannot satisfy a sustained
  qualification requirement.
- Existing terminal Runs retain their historical diagnostic evidence.
- One-shot Policies may continue accepting trustworthy legacy one-shot Runs
  where duration is irrelevant.
- New sustained qualification requires explicit Continuous or Periodic stage
  evidence.

CRD lists, maps, and strings use conservative nested and aggregate bounds so
ordinary fully populated Runs and evidence identities remain within normal
Kubernetes object-storage limits. These bounds are implementation guardrails,
not the Project Signal abstraction.

Non-pass watermarks, fingerprint epochs, expiration, revocation, exact Node UID
binding, and protected-label AND semantics are unchanged.

## Required runtime follow-through

The contracts require coordinated upper-layer implementation:

- The controller resolves tests and mutable inputs, validates trust, freezes
  Profile bindings, computes the plan digest, constructs phase concurrency, and
  evaluates stage and Run verdicts.
- The adapter and runner own continuous and periodic scheduling, invocation
  timeouts, bounded retries, workload sample collection, resume state, and
  immutable result envelopes.
- Deployment configures approved registries, templates, resolvers, capabilities,
  mounts, namespaces, quotas, runtime classes, and qualification security gates.

Qualification projection must remain disabled until these behaviors and the
NodeRestriction and Node-patch RBAC protections are independently verified.

Qualification host paths are limited to read-only approved proc leaves,
read-only sysfs paths, or the dedicated `/var/lib/project-signal` data root.
They cannot replace or shadow the reserved `/scripts` and `/results` mounts.

## Generic workload fixtures

The contract intentionally does not define vLLM, storage, network, MPI, or NCCL
API kinds. Those workloads are approved tests built from the same three generic
drivers.

The vLLM fixture in
`api/signal/v1alpha1/generic_contract_fixture_test.go` uses an approved
`WorkflowTemplate` test with typed image channel, model and revision, workload
shape, duration, concurrency, and threshold parameters. Its metric schema
covers request and token throughput, TTFT, TPOT and latency percentiles,
request and error counts, model identity, and GPU coverage. The Suite binds
declarative assertions and a short `RunOnce` schedule; the same test identity is
also valid under bounded `Continuous` and `Periodic` schedules.

Before qualification, mutable image, model, and tokenizer requests are resolved
to immutable revisions and digests in the Run. A resolver that cannot produce
an authoritative digest records an explicit `Incomplete` input with a bounded
reason. Such a stage is diagnostic-only and cannot grant qualification credit.
Changing resolved image or model identity, workload shape, assertions, or
schedule changes `planDigest`.

The existing 15-script baseline migrates without a monolithic wrapper. Each
script becomes one approved immutable `Script` test, and one ordered Suite
phase retains the exact operator name and position of all 15 stages. Its
historical Runs and evidence remain on the legacy representation. New baseline
and vLLM qualification should use separate Suites and Runs so resources,
cleanup, evidence, and failures remain isolated; a `CertificationPolicy` may
require both. Complete invocation payloads and cleanup details remain in
bounded durable result artifacts rather than CR status.
