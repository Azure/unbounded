#!/usr/bin/env bash
# Gantry vs Gantry+Racer pull benchmark harness.
#
# Subcommands:
#   prep                 install containerd hosts.toml for the bench registry on every worker
#   gen <name> <layers> <layer-bytes>
#                        push a fresh random image to the origin, print repo@digest
#   run <label> <repo@digest> <mode> [concurrency]
#                        start every worker pulling at the same instant and collect results
#                        mode=crictl: full containerd pull + unpack
#                        mode=curl:   fetch every layer blob through the local gantry mirror
#   summarize <results.tsv>
set -euo pipefail

NS=gantry-bench
HOST=bench.registry.local
ORIGIN=${ORIGIN:-10.0.90.91:5000}
DIR=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-$DIR/../../tmp/gantry-racer-bench}
LEAD=${LEAD:-240}
BUSYBOX=mcr.microsoft.com/azurelinux/busybox:1.36

mkdir -p "$OUT"

# Refuse to touch a cluster other than the one named in BENCH_CONTEXT. The
# harness deletes images and writes host files on every node it can reach.
if [[ -z ${BENCH_CONTEXT:-} || $(kubectl config current-context) != "$BENCH_CONTEXT" ]]; then
	echo "set BENCH_CONTEXT to the current kubectl context to confirm the target cluster" >&2
	exit 1
fi

origin_tx() {
	kubectl -n "$NS" exec "$(tools_pod)" -- wget -qO- http://origin:5001/metrics |
		awk '$1 == "process_network_transmit_bytes_total" {printf "%.0f\n", $2}'
}

tools_pod() {
	kubectl -n "$NS" get pod -l app=tools -o jsonpath='{.items[0].metadata.name}'
}

worker_affinity() {
	cat <<'EOF'
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kubernetes.azure.com/mode
                    operator: In
                    values: ["user"]
EOF
}

cmd_prep() {
	kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: prep
  namespace: $NS
spec:
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 100%
  selector:
    matchLabels:
      app: prep
  template:
    metadata:
      labels:
        app: prep
    spec:
      terminationGracePeriodSeconds: 1
$(worker_affinity)
      tolerations:
        - operator: Exists
      containers:
        - name: prep
          image: $BUSYBOX
          securityContext:
            privileged: true
          command:
            - chroot
            - /host
            - bash
            - -c
            - |
              set -e
              mkdir -p /etc/containerd/certs.d/$HOST
              cat > /etc/containerd/certs.d/$HOST/hosts.toml <<'TOML'
              # Fall back to the origin itself, as a real registry host would.
              server = "http://$ORIGIN"
              [host."http://127.0.0.1:5000"]
                capabilities = ["pull", "resolve"]
              TOML
              echo done
              exec sleep infinity
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
          volumeMounts:
            - name: host
              mountPath: /host
      volumes:
        - name: host
          hostPath:
            path: /
EOF
	kubectl -n "$NS" rollout status ds/prep --timeout=15m
}

# cmd_diskprep frees the Azure resource disk (mounted at /mnt) on every worker
# that has one, zeroes its head and tail so racer's device guard accepts it,
# and annotates the Node so racer uses it as a raw block device.
cmd_diskprep() {
	kubectl -n "$NS" delete ds diskprep --ignore-not-found --wait=true >/dev/null
	kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: diskprep
  namespace: $NS
spec:
  selector:
    matchLabels:
      app: diskprep
  template:
    metadata:
      labels:
        app: diskprep
    spec:
$(worker_affinity)
      hostPID: true
      terminationGracePeriodSeconds: 1
      tolerations:
        - operator: Exists
      containers:
        - name: diskprep
          image: $BUSYBOX
          securityContext:
            privileged: true
          command: ["chroot", "/host", "nsenter", "-t", "1", "-m", "-u", "-i", "-n", "--", "bash", "-c"]
          args:
            - |
$(sed 's/^/              /' "$DIR/diskprep.sh")
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
          volumeMounts:
            - name: host
              mountPath: /host
      volumes:
        - name: host
          hostPath:
            path: /
EOF
	kubectl -n "$NS" rollout status ds/diskprep --timeout=15m
	kubectl -n "$NS" get pods -l app=diskprep -o name |
		xargs -P 64 -I{} sh -c "kubectl -n $NS logs {} | grep '^DISK' || true" >"$OUT/disks.txt"
	awk '$3 != "none" {print $2, $3}' "$OUT/disks.txt" |
		xargs -P 64 -n 2 sh -c 'kubectl annotate node "$0" --overwrite "racer.unbounded-cloud.io/block-devices=^$1\$" >/dev/null'
	awk '{print $3 == "none" ? $4 : "device"}' "$OUT/disks.txt" | sort | uniq -c
	kubectl -n "$NS" delete ds diskprep --wait=false >/dev/null
}

cmd_gen() {
	local name=$1 layers=$2 bytes=$3
	kubectl -n "$NS" exec "$(tools_pod)" -- /tmp/genimage -registry http://origin:5000 \
		-repo "bench/$name" -layers "$layers" -layer-bytes "$bytes" -parallel 8
}

cmd_run() {
	local label=$1 ref=$2 mode=$3 conc=${4:-3}
	local repo=${ref%@*} digest=${ref#*@}
	local start_at=$(($(date +%s) + LEAD))
	local name="pull-$(date +%s)"
	local layers=""

	if [ "$mode" = curl ]; then
		layers=$(kubectl -n "$NS" exec "$(tools_pod)" -- wget -qO- \
			--header 'Accept: application/vnd.oci.image.manifest.v1+json' \
			"http://origin:5000/v2/$repo/manifests/$digest" |
			python3 -c 'import json,sys; print(" ".join(l["digest"] for l in json.load(sys.stdin)["layers"]))')
	fi

	local tx0
	tx0=$(origin_tx)

	echo "run=$label ds=$name start_at=$start_at ($(date -d @"$start_at" -u +%T)) ref=$ref mode=$mode" >&2

	kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: $name
  namespace: $NS
  labels:
    app: pull
spec:
  selector:
    matchLabels:
      app: pull
      run: $name
  template:
    metadata:
      labels:
        app: pull
        run: $name
    spec:
$(worker_affinity)
      hostNetwork: true
      tolerations:
        - operator: Exists
      terminationGracePeriodSeconds: 1
      containers:
        - name: pull
          image: $BUSYBOX
          securityContext:
            privileged: true
          env:
            - name: NODE
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: START_AT
              value: "$start_at"
            - name: REF
              value: "$HOST/$repo@$digest"
            - name: REPO
              value: "$repo"
            - name: MODE
              value: "$mode"
            - name: LAYERS
              value: "$layers"
            - name: CONC
              value: "$conc"
          command:
            - chroot
            - /host
            - bash
            - -c
            - |
              set -u
              CRI="crictl --runtime-endpoint unix:///run/containerd/containerd.sock --image-endpoint unix:///run/containerd/containerd.sock --timeout 2h"
              # Drop images from earlier rounds so the disk does not fill.
              for id in \$(\$CRI images 2>/dev/null | awk '\$1 ~ /^$HOST\// {print \$3}' | sort -u); do
                \$CRI rmi "\$id" >/dev/null 2>&1 || true
              done
              echo "READY \$NODE \$(date +%s.%N)"
              while [ "\$(date +%s)" -lt "\$START_AT" ]; do sleep 0.02; done
              t0=\$(date +%s.%N)
              rc=0
              bytes=0
              if [ "\$MODE" = crictl ]; then
                out=\$(\$CRI pull "\$REF" 2>&1) || rc=\$?
              else
                tmp=\$(mktemp)
                printf '%s\n' \$LAYERS | xargs -P "\$CONC" -I{} curl -sS -o /dev/null -w '%{http_code} %{size_download}\n' \
                  "http://127.0.0.1:5000/v2/\$REPO/blobs/{}?ns=$HOST" > "\$tmp" 2>&1 || rc=\$?
                bytes=\$(awk '{s+=\$2} END {print s+0}' "\$tmp")
                bad=\$(awk '\$1 != 200' "\$tmp" | wc -l)
                [ "\$bad" -eq 0 ] || rc=99
                out=\$(head -c 300 "\$tmp" | tr '\n' ' ')
                rm -f "\$tmp"
              fi
              t1=\$(date +%s.%N)
              echo "RESULT \$NODE \$MODE \$t0 \$t1 \$rc \$bytes"
              [ "\$rc" -eq 0 ] || echo "ERROR \$NODE \$(echo "\$out" | tr '\n' ' ' | head -c 500)"
              exec sleep infinity
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
          volumeMounts:
            - name: host
              mountPath: /host
      volumes:
        - name: host
          hostPath:
            path: /
EOF

	local results="$OUT/$label.tsv"
	local want
	want=$(kubectl get nodes -l kubernetes.azure.com/mode=user --no-headers | wc -l)

	# Wait for every pod to report a result.
	while true; do
		sleep 20
		collect "$name" >"$results.tmp"
		local got
		got=$(grep -c '^RESULT' "$results.tmp" || true)
		echo "$(date -u +%T) results $got/$want" >&2
		if [ "$got" -ge "$want" ]; then
			break
		fi
		if [ "$(date +%s)" -gt $((start_at + ${MAX_WAIT:-3600})) ]; then
			echo "timed out waiting for results" >&2
			break
		fi
	done

	mv "$results.tmp" "$results"
	local tx1
	tx1=$(origin_tx)
	echo "ORIGIN_TX $((tx1 - tx0))" >>"$results"
	late=$(awk -v s="$start_at" '$1 == "READY" && $3 > s' "$results" | wc -l)
	echo "pods ready after start: $late" >&2
	kubectl -n "$NS" delete ds "$name" --wait=false >/dev/null
	cmd_summarize "$results" "$start_at"
}

# collect prints READY/RESULT/ERROR lines from every pod of a run.
collect() {
	local name=$1
	kubectl -n "$NS" get pods -l run="$name" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' |
		xargs -P 64 -I{} sh -c "kubectl -n $NS logs {} 2>/dev/null | grep -E '^(READY|RESULT|ERROR)' || true"
}

cmd_summarize() {
	local results=$1 start_at=${2:-}
	python3 - "$results" "$start_at" <<'EOF'
import sys, statistics
path, start_at = sys.argv[1], sys.argv[2]
rows, errors = [], []
for line in open(path):
    f = line.split()
    if f[0] == "RESULT":
        rows.append((f[1], float(f[3]), float(f[4]), int(f[5]), int(f[6])))
    elif f[0] == "ORIGIN_TX":
        print(f"origin egress: {int(f[1]) / 2**30:.2f} GiB")
    elif f[0] == "ERROR":
        errors.append(line.strip())
if not rows:
    print("no results"); sys.exit()
t0 = float(start_at) if start_at else min(r[1] for r in rows)
def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]
def line(name, rs):
    d = [r[2] - t0 for r in rs if r[3] == 0]
    fails = sum(1 for r in rs if r[3] != 0)
    if not d:
        print(f"{name:10s} n={len(rs)} failed={fails}"); return
    print(f"{name:10s} n={len(rs):5d} ok={len(d):5d} failed={fails:4d} "
          f"p50={pct(d,50):7.1f}s p90={pct(d,90):7.1f}s p95={pct(d,95):7.1f}s p99={pct(d,99):7.1f}s "
          f"max={max(d):7.1f}s mean={statistics.mean(d):7.1f}s")
line("all", rows)
for pool in sorted({r[0].split("-")[1] for r in rows}):
    line(pool, [r for r in rows if r[0].split("-")[1] == pool])
skew = [r[1] - t0 for r in rows]
print(f"start skew: max {max(skew):.2f}s")
for e in errors[:5]:
    print(e)
EOF
}

case ${1:-} in
prep) cmd_prep ;;
diskprep) cmd_diskprep ;;
gen) shift; cmd_gen "$@" ;;
run) shift; cmd_run "$@" ;;
summarize) shift; cmd_summarize "$@" ;;
*) sed -n '2,13p' "$0"; exit 1 ;;
esac
