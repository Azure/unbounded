#!/usr/bin/env bash
# Sum Prometheus counters across all gantry (:9095) and racer-dataplane (:9090)
# pods. Runs the scrape from the tools pod in gantry-bench.
# Usage: scrape.sh <out-prefix>   -> <out-prefix>.gantry.prom, <out-prefix>.racer.prom
set -euo pipefail
out=$1
ns=unbounded-system
tools=$(kubectl -n gantry-bench get pod -l app=tools -o name | cut -d/ -f2)
scrape() {
  local label=$1 port=$2 file=$3
  kubectl -n $ns get pods -l app.kubernetes.io/name=$label -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' >"$file.ips"
  kubectl -n gantry-bench cp "$file.ips" "$tools:/tmp/$label.ips" >/dev/null
  kubectl -n gantry-bench exec "$tools" -- sh -c "rm -rf /tmp/m-$label; mkdir -p /tmp/m-$label; for ip in \$(cat /tmp/$label.ips); do wget -qO /tmp/m-$label/\$ip -T 5 http://\$ip:$port/metrics & if [ \$(jobs | wc -l) -ge 64 ]; then wait; fi; done; wait; cat /tmp/m-$label/*" 2>/dev/null |
    grep -v '^#' | sed -E 's/(disk|worker|device)="[^"]*",?//g; s/\{\}//' | awk 'NF==2 {s[$1]+=$2} END {for (k in s) printf "%s %.0f\n", k, s[k]}' | sort >"$file"
  rm -f "$file.ips"
}
scrape gantry 9095 "$out.gantry.prom" &
scrape racer-dataplane 9090 "$out.racer.prom" &
wait
