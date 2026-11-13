---
title: "Project Signal certification plans"
weight: 40
description: "Generate immutable Project Signal Tests and ordered Suites from repository-owned source files."
---

`CertificationPlan` is the repository authoring format for Project Signal
tests. It references source files and named digest-pinned runtimes; it is not a
Kubernetes resource.

For a plan that defines only Tests, render it with:

```bash
kubectl unbounded signal render \
  --filename certification-plan.yaml \
  > certification-tests.yaml
```

The command reads each source file relative to `spec.sourceRoot`, validates
that it stays inside that root, computes its SHA-256 digest, and embeds the
exact content in the generated `CertificationTest`. The output is deterministic
and can be committed for GitOps or applied directly.

```yaml
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: node-qualification
  labels:
    app.kubernetes.io/part-of: project-signal
spec:
  sourceRoot: .
  owner: qualification-team
  version: source-commit-or-release
  runtimes:
    base: registry.example/signal-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    vllm: registry.example/vllm-runner@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
  tests:
  - name: gpu-identity
    displayName: GPU identity
    artifacts:
    - name: runner.py
      path: scripts/runner.py
    - name: gpu-identity.sh
      path: scripts/gpu-identity.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
        artifact: gpu-identity.sh
        resources:
          requests:
            nvidia.com/gpu: "8"
          limits:
            nvidia.com/gpu: "8"
  - name: vllm-canary
    displayName: vLLM canary
    driver:
      type: Container
      container:
        runtime: vllm
        command: [python3]
        args: [-m, vllm.entrypoints.openai.api_server]
  suite:
    name: node-qualification
    phases:
    - name: validation
      displayName: Ordered node validation
      role: Validation
      stages:
      - name: gpu-identity
        test: gpu-identity
        schedule:
          mode: RunOnce
          invocationTimeout: 10m
      - name: vllm-canary
        test: vllm-canary
        schedule:
          mode: RunOnce
          invocationTimeout: 30m
        dependsOn:
        - stage: gpu-identity
```

Use singular `artifact` for a one-file Test. Use `artifacts` when the selected
entrypoint needs supporting files. Every file is embedded with its own digest,
and a multi-artifact Script or WorkflowTemplate driver must select its
entrypoint by artifact name. Script Pods materialize every approved artifact,
but execute only the selected entrypoint. Container drivers do not accept
artifacts.

Supported drivers are:

- `Script`, with one selected artifact, `Shell` or `Python` language, and a
  named runtime.
- `Container`, with a named runtime plus optional command and arguments.
- `WorkflowTemplate`, with a template name, canonical SHA-256 digest, and an
  optional artifact passed through the standard script parameters.

When omitted, topology defaults to one participant and metrics default to the
required `scriptResult` status metric. Runtime images and WorkflowTemplate
digests must be immutable SHA-256 references.

## Ordered Suite workflow

A Suite pins each accepted Test by Kubernetes UID, generation, and approved
content digest. Those identities do not exist until the generated Tests have
been applied and approved, so Suite compilation is intentionally two-phase.
This keeps the source workflow repeatable without weakening the qualification
trust boundary.

First render and apply only the Tests:

```bash
kubectl unbounded signal render \
  --filename certification-plan.yaml \
  --tests-only \
  > certification-tests.yaml

kubectl apply -f certification-tests.yaml
```

After an administrator approves the exact Test generations, capture their
cluster identities:

```bash
kubectl unbounded signal bindings \
  --filename certification-plan.yaml \
  > certification-bindings.yaml
```

The command fails closed if a Test is missing, its current generation has not
been observed, or its trust state is not `Approved` or `Diagnostic`. The
generated file has this shape:

```yaml
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: node-qualification
spec:
  tests:
    gpu-identity:
      name: gpu-identity
      uid: 11111111-1111-1111-1111-111111111111
      generation: 1
      contentDigest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    vllm-canary:
      name: vllm-canary
      uid: 22222222-2222-2222-2222-222222222222
      generation: 1
      contentDigest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
```

Then compile the complete apply-ready package:

```bash
kubectl unbounded signal render \
  --filename certification-plan.yaml \
  --bindings certification-bindings.yaml \
  > certification-package.yaml

kubectl apply -f certification-package.yaml
```

Environment-specific values can remain outside the readable plan in a
`CertificationPlanInputs` file. Inputs override same-named stage parameters
and fail closed if they name a stage that is not in the plan:

```yaml
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanInputs
metadata:
  name: node-qualification
spec:
  stages:
    gpu-identity:
      parameters:
      - name: environmentJson
        value:
          type: String
          stringValue: '{"EXPECTED_GPU_COUNT":"8"}'
```

Pass the overlay only with approved bindings:

```bash
kubectl unbounded signal render \
  --filename certification-plan.yaml \
  --bindings certification-bindings.yaml \
  --inputs certification-inputs.yaml \
  > certification-package.yaml
```

The complete package contains the deterministic `CertificationTest` objects
followed by the bound `CertificationSuite`. Keep the package as the deployment
and cleanup artifact:

```bash
kubectl delete -f certification-package.yaml
```

Changing a script, runtime, parameter schema, metric schema, security
requirement, or other Test content produces a new Test generation and requires
fresh approval and bindings before the Suite can be recompiled.
