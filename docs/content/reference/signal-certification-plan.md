---
title: "Project Signal certification plans"
weight: 40
description: "Generate immutable CertificationTests from repository-owned source files."
---

`CertificationPlan` is the repository authoring format for Project Signal
tests. It references source files and named digest-pinned runtimes; it is not a
Kubernetes resource.

Render the plan with:

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
    artifact:
      path: scripts/gpu-identity.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
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
```

Supported drivers are:

- `Script`, with an artifact, `Shell` or `Python` language, and named runtime.
- `Container`, with a named runtime plus optional command and arguments.
- `WorkflowTemplate`, with a template name, canonical SHA-256 digest, and an
  optional artifact passed through the standard script parameters.

When omitted, topology defaults to one participant and metrics default to the
required `scriptResult` status metric. Runtime images and WorkflowTemplate
digests must be immutable SHA-256 references.
