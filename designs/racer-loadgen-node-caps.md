# Optional load generator node caps

Keep the existing scalar `concurrency` key and global pause command unchanged.
Add a second key to the same projected ConfigMap:

```json
{"version":1,"caps":{"aks-ddsv6-84072342-vmss00000h":1}}
```

Enable only on approved readers with
`--node-concurrency-caps-file=/etc/loadgen-control/node-caps.json` and
`--node-name=$(NODE_NAME)`, using a Downward API `NODE_NAME` environment variable
from `spec.nodeName`. Keep `--concurrency-file=/etc/loadgen-control/concurrency`
and startup fallback `--concurrency=0`. Mount the whole ConfigMap directory, not
subPath keys. Both keys must share its `..data` projection; ordinary independent
files are not accepted in cap mode. Old readers ignore the additional key.

Effective concurrency is min(global, matching cap); no match in a valid map means
no additional cap. Global zero wins even with invalid caps. Initial errors keep
admissions zero. Later errors retain the last valid cap and may only decrease
effective concurrency; a valid complete read is required to resume or increase.
A missing file is an error, not cap removal. Explicitly remove an entry from a
valid map to release its cap. Limits are 64 KiB and 256 entries, caps 0-256,
exact Kubernetes node names, version 1 only, no duplicate/unknown keys or trailing
JSON. Each poll pins one ConfigMap generation; deletion of an old generation
fails safely. A separately read scalar may only lower authority on read failure.

Applied concurrency reports the effective admission limit. Already admitted
pulls drain under their existing timeout. Origins, catalog generation, image
selection, verification, byte accounting, and layer concurrency are unchanged.
No changes are made to deployment manifests by this feature. A parent-owned
targeted deployment must guard the existing loadgen DaemonSet as OnDelete before
changing its template, pause/drain using the existing global control, and replace
only approved pods. Installing new code requires those pods to restart once;
future cap updates do not restart origins. Do not restore RollingUpdate with an
unreviewed divergent template. Never exclude capped nodes from fleet accounting.
