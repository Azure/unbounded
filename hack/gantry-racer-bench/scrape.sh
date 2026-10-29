#!/usr/bin/env bash
# Sum Prometheus counters across all gantry (:9095) and racer-dataplane (:9090)
# pods. Runs the scrape from the tools pod in gantry-bench. Fails rather than
# writing a partial fleet total when any pod cannot be scraped.
# Usage: scrape.sh <out-prefix>   -> <out-prefix>.gantry.prom, <out-prefix>.racer.prom
set -euo pipefail
out=$1
ns=unbounded-system
tools=$(kubectl -n gantry-bench get pod -l app=tools -o name | cut -d/ -f2)
scrape() {
  local label=$1 port=$2 file=$3
  kubectl -n $ns get pods -l app.kubernetes.io/name=$label -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' >"$file.ips"
  kubectl -n gantry-bench cp "$file.ips" "$tools:/tmp/$label.ips" >/dev/null
  rm -f "$file.ips"
  # Each download gets its final name only on success, so the file count
  # shows whether every pod was scraped. The IP file has one line per pod,
  # blank for a pod without an IP yet, so count every line.
  if ! kubectl -n gantry-bench exec "$tools" -- sh -c "rm -rf /tmp/m-$label; mkdir -p /tmp/m-$label
for ip in \$(cat /tmp/$label.ips); do
  (wget -qO /tmp/m-$label/\$ip.part -T 5 http://\$ip:$port/metrics && mv /tmp/m-$label/\$ip.part /tmp/m-$label/\$ip.prom) &
  if [ \$(jobs | wc -l) -ge 64 ]; then wait; fi
done
wait
want=\$(grep -c '' /tmp/$label.ips)
got=\$(ls /tmp/m-$label | grep -c '\.prom\$')
if [ \"\$got\" -ne \"\$want\" ]; then
  echo \"$label: scraped \$got of \$want pods\" >&2
  exit 1
fi
if [ \"\$want\" -gt 0 ]; then cat /tmp/m-$label/*.prom; fi" >"$file.raw"; then
    rm -f "$file.raw"
    return 1
  fi
  { grep -v '^#' "$file.raw" || true; } | sed -E 's/(disk|worker|device)="[^"]*",?//g; s/\{\}//' | awk 'NF==2 {s[$1]+=$2} END {for (k in s) printf "%s %.0f\n", k, s[k]}' | sort >"$file"
  rm -f "$file.raw"
}
scrape gantry 9095 "$out.gantry.prom" &
gantry=$!
scrape racer-dataplane 9090 "$out.racer.prom" &
racer=$!
rc=0
wait "$gantry" || rc=1
wait "$racer" || rc=1
exit "$rc"
