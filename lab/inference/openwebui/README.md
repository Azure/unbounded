# W1.1 - Open WebUI customer-facing chat UI

Wave item: **W1.1** (see [`../../../plan.md`](../../../plan.md)).

Deploys [Open WebUI](https://github.com/open-webui/open-webui) (BSD-3) at the
root path of the lab's public hostname. It talks to the in-cluster Ollama
service over the cluster network, so the chat UI never round-trips through
the public ingress and never carries Ollama's basic-auth credentials.

## What it proves

- A customer can hit a single URL, sign up, and chat with the
  Spark-resident model.
- An off-the-shelf chat front end runs on the AKS system pool while the
  model engine runs on a Region-A DGX Spark, talking to each other across
  unbounded-net via cluster-internal DNS (no public hop, no auth header).
- Path-based ingress sharing on a single Azure-managed DNS label: root
  serves the UI; `/ollama/` serves the API.

## Files

| File | Role |
|---|---|
| `namespace.yaml` | `lab-openwebui` namespace |
| `pvc.yaml` | Azure Disk PVC (`default` SC), 10 GiB, holds chat history + embedding cache |
| `service.yaml` | ClusterIP `open-webui` :8080 |
| `deployment.yaml` | Single-replica Deployment, pinned to AKS amd64 system pool |
| `ingress.yaml` | nginx Ingress at host root, cert-manager TLS, no basic auth |
| `make-secrets.sh` | Generates `secret.local.yaml` with a fresh `WEBUI_SECRET_KEY`. Re-run to rotate sessions. |
| `secret.local.yaml` | Generated; gitignored. |
| `kustomization.yaml` | Bundles everything. |

## Hostname and routing

The public hostname is operator-supplied via the `configMapGenerator`
(default placeholder: `chat.lab.example.com`). On Azure-managed DNS labels
(`*.<region>.cloudapp.azure.com`) only one label per public IP is allowed,
so Open WebUI and the Ollama API share a single hostname via path-based
routing:

| Path | Backend |
|---|---|
| `/` | Open WebUI (this manifest set) |
| `/ollama/*` | Ollama API in `lab-ollama-qwen-moe` (basic auth) |

The Ollama side rewrites away the `/ollama` prefix before forwarding.

## Why pinned to AKS, not a Spark

The PVC uses Azure Disk (RWO, attaches only to AKS-managed VMs). Open WebUI
is a light non-GPU workload, so we keep it on the cheap AKS system pool and
leave the Sparks for inference engines. Cross-node Service traffic to the
Spark-resident Ollama pod traverses unbounded-net.

## Deploy

```sh
make LAB_HOST=mychat.example.com lab-w1.1-openwebui-up
```

Idempotent. `LAB_HOST` overrides the `open-webui-host` configMap on the
fly (the target backs up and restores `kustomization.yaml`, even on
Ctrl-C); without it, the placeholder `chat.lab.example.com` is used and
the public endpoint will not work. Generates `secret.local.yaml` on first
run. Re-running does NOT rotate the session key; delete
`secret.local.yaml` and re-run to rotate (invalidates all logged-in
sessions).

## First use

1. Browse to your configured chat hostname (default placeholder:
   `https://chat.lab.example.com/`).
2. Sign up. **The first signup becomes admin.**
3. Subsequent signups land in `pending`. The admin approves them under
   `Admin Panel -> Users`.
4. The model picker should already show `qwen3:30b-a3b`. Open WebUI auto-
   discovers it via `OLLAMA_BASE_URL=http://ollama.lab-ollama-qwen-moe.svc.cluster.local:11434`.

## Status

```sh
make lab-openwebui-status
```

## Teardown

```sh
make lab-openwebui-down
# Deletes the namespace AND the chat-history PVC. Sign up state is lost.
```

## Known limitations

- **Single replica.** Recreate strategy means a brief outage during rollouts.
- **No external auth.** Anyone can hit the signup page until you disable
  signups (set `ENABLE_SIGNUP=false` after the admin and approved users are
  created). W1.4 will replace this with the shared auth proxy.
- **Admission policy is manual.** `DEFAULT_USER_ROLE=pending` blocks new
  signups from chatting until the admin approves them. There is no email
  notification configured.
## Future hooks

- W1.2 will add vLLM as a second engine. Open WebUI can connect to multiple
  OpenAI-compatible backends; the model picker will then offer both engines
  side-by-side for the same Qwen3 30B-A3B weights.
- W1.4 will move auth out of nginx basic-auth and Open WebUI's signup flow
  into a shared OIDC/OAuth2 proxy.
