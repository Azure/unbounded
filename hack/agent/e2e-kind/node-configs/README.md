# Agent e2e node configs

This folder contains agent config scenarios used by the agent e2e tests. Each
JSON file can be passed to `e2e.py` with `--node-config`.

| Field | Type | Description |
| --- | --- | --- |
| `name` | string | Scenario name used for logs and per-scenario VM names. Defaults to the JSON file stem when omitted. |
| `nodeLabels` | object | Kubernetes node labels to pass to `manual-bootstrap --node-label`. Keys and values must be strings. |
| `registerWithTaints` | array of strings | Kubernetes taints to pass to `manual-bootstrap --register-with-taint`. Each value uses `key[=value]:Effect` format. |
| `nodeIP` | string | Optional node IP to pass to `manual-bootstrap --node-ip`. Use `$VM_IP` or `${VM_IP}` to use the scenario VM address. |
| `offlineArtifactsOCIRef` | string | Optional OCI artifact bundle reference for offline bootstrap binaries. The e2e mirrors it into a local registry and passes the local `oci://` reference to `manual-bootstrap --offline-artifacts-source`. For blocked-network scenarios, the e2e builds local bundles during the test for both the bootstrap Kubernetes version and the repave target version. Those bundles include cluster-only kube-system images plus the e2e workload image, then the e2e passes a templated local bundle reference. |
| `offlineRootfsOCIImage` | string | Optional rootfs OCI image reference. The e2e mirrors it into a local registry and passes the local reference to `manual-bootstrap --oci-image`. |
| `blockExternalNetwork` | boolean | Optional. When true, the e2e installs required host packages, then blocks VM egress outside local e2e networks before running bootstrap. This is intended for offline bootstrap validation. The offline artifact bundle includes kube-system images needed for node readiness plus the e2e workload image. |
| `localDNS` | boolean | Enables the nspawn-local CoreDNS cache and validates service health, resolver wiring, metrics, listener addresses, and NOTRACK rules. The scenario must also provide `nodeIP` so metrics bind to the Node InternalIP. |
| `additionalHostMounts` | array of objects | Optional extra host bind-mounts for the nspawn machine. Each entry has a required string `source` (clean absolute host path), optional string `target` (defaults to `source`), and optional bool `readOnly`. The e2e passes each entry as `--additional-host-mount` to `manual-bootstrap` and validates the resulting `Bind=` / `BindReadOnly=` directives in the nspawn config. |

The `validate-node-configs` parent process prepares OCI refs once, then passes the
local refs to each child `e2e.py` invocation with
`--offline-artifacts-oci-ref` and `--offline-rootfs-oci-image`.
