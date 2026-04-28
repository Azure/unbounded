# W1.1 - Ollama serving Qwen MoE on spark-3d37

Wave item: **W1.1** (see [`../../../plan.md`](../../../plan.md)).

Deploys an Ollama server pinned to `spark-3d37` (Region A, GB10) with weights on
local-path PVC, fronted by an nginx Ingress with TLS (cert-manager / Let's
Encrypt) and HTTP basic auth.

The Ollama API is mounted under the `/ollama/` path prefix on the public
hostname so the root path can host a customer-facing chat UI (Open WebUI,
see [`../openwebui/`](../openwebui/)). The nginx ingress strips the `/ollama`
prefix before forwarding, so the upstream Ollama server still sees its
native paths (`/api/...`, `/v1/...`).

The basic-auth pattern here is a W1.1 stand-in. **W1.4** consolidates the
shared ingress + auth pattern across all Wave 1/2 engines.

## What it proves

- Ollama runs on ARM64 + GB10 (sm_120) under the `nvidia` RuntimeClass.
- Local-path PVC pins weights to the node; survives pod restarts; does not
  survive PVC deletion or node loss (W1.3 measures both).
- Engine parameters configured via env vars on the container, not a
  ConfigMap (Ollama reads env, not a config file).
- Standard k8s ingress + cert-manager + basic-auth Secret reach an AI
  workload on an edge ARM64 GPU node.

## Files

| File | Role |
|---|---|
| `namespace.yaml` | `lab-ollama-qwen-moe` namespace |
| `statefulset.yaml` | Single-replica StatefulSet pinned to `spark-3d37` |
| `service.yaml` | ClusterIP `ollama` and headless `ollama-headless` |
| `ingress.yaml` | nginx Ingress + cert-manager + basic auth |
| `kustomization.yaml` | Bundles everything; sets the public hostname via a ConfigMap-driven replacement |
| `make-auth-secret.sh` | Generates `secret.local.yaml` with a fresh APR1 password hash. Re-run to rotate. |
| `secret.local.yaml` | Generated; gitignored. Holds the htpasswd Secret. |

## Hostname

`ollama.lab.example.com` is a placeholder. The Make targets accept
`LAB_HOST=<fqdn>` to override on the fly without committing the literal:

```sh
make LAB_HOST=mychat.example.com lab-w1.1-ollama-up
```

The target backs up `kustomization.yaml`, runs `kustomize edit set
configmap ollama-host --from-literal=host=$LAB_HOST`, applies, and
restores the file even on Ctrl-C. The host must already resolve to the
`ingress-nginx-controller` LoadBalancer IP before cert-manager can solve
the ACME HTTP-01 challenge.

For a permanent change (e.g. forking the lab) edit the literal directly:

```sh
cd lab/inference/ollama-qwen-moe
kustomize edit set configmap ollama-host --from-literal=host=<new>
```

## Deploy

```sh
make lab-w1.1-up
```

The Make target generates the auth secret on first run and saves credentials
to stdout (record them in your password manager; they are not stored in the
repo). Re-running `make lab-w1.1-up` does NOT rotate the password; delete
`secret.local.yaml` and re-run the target to rotate.

Equivalent manual flow:

```sh
( cd lab/inference/ollama-qwen-moe && ./make-auth-secret.sh )
kubectl apply -k lab/inference/ollama-qwen-moe
kubectl -n lab-ollama-qwen-moe rollout status statefulset/ollama --timeout=10m
```

Pull the model into the PVC. This is the W1.3 cold-start measurement step;
keep the timing output:

```sh
USER=lab; PASS=<from make-auth-secret.sh>
START=$(date -u +%s)
curl -sS -u "$USER:$PASS" \
  -X POST https://ollama.lab.example.com/ollama/api/pull \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3:30b-a3b","stream":true}' \
  -o /tmp/ollama-pull.ndjson
echo "DURATION: $(( $(date -u +%s) - START ))s"
```

Smoke test:

```sh
curl -sS -u "$USER:$PASS" https://ollama.lab.example.com/ollama/api/generate \
  -d '{"model":"qwen3:30b-a3b","prompt":"hello","stream":false}' | jq -r .response
```

## Status

```sh
make lab-w1.1-status
```

## Teardown

```sh
make lab-w1.1-down
# The PVC is deleted with the namespace. Re-deploying triggers a fresh pull.
```

## Pain measurement runbook (W1.3)

Record results in [`../../storage-pain-journal.md`](../../storage-pain-journal.md).

1. **Time to first inference after cold pod start.** Two variants:
   - Warm-PVC (pod restart, weights survive). `kubectl -n lab-ollama-qwen-moe delete pod ollama-0`; measure pod ready + first `/api/generate` reply.
   - Cold-PVC (PVC recreated). `make lab-w1.1-down && make lab-w1.1-up`; then run the timed pull above.
2. **Origin egress per pod start.** Read the final `total` byte count from
   the pull NDJSON (`tail -1 /tmp/ollama-pull.ndjson | jq .total`); for
   `qwen3:30b-a3b` Q4_K_M this is ~18.6 GB. To verify on the wire, run on
   `spark-3d37` while the pull is in flight:
   `sudo tcpdump -i any -w /tmp/ollama-pull.pcap host registry.ollama.ai`,
   then `capinfos -b /tmp/ollama-pull.pcap`.
3. **Cold start after node reboot (PVC survives).** Reboot
   `spark-3d37`; measure pod ready time after node returns; should be
   << first-pull time.
4. **Disk footprint.**
   ```sh
   kubectl debug node/spark-3d37 -it --image=alpine \
     -- du -sh /host/opt/local-path-provisioner
   ```
   (or read the pulled byte total from the NDJSON).

## Known limitations

- **Single replica, single GPU.** No HA. Replacing the node loses the PVC.
- **Basic auth, not the W1.4 shared proxy.** Sufficient for an internal lab;
  not the production auth story.
- **No automated model pull.** The first pull is operator-driven so we get
  clean wall-clock numbers for W1.3. After the first pull the weights persist
  in the PVC and pod restarts are fast.
