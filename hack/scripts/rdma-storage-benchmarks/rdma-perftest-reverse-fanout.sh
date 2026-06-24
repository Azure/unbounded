#!/usr/bin/env bash
set -euo pipefail

VM0="azureuser@20.120.39.72"
VM1="azureuser@172.173.244.140"
BASE="/home/azureuser/ub-rdma-bench"
DEVS=(mlx5_0 mlx5_1 mlx5_2 mlx5_3 mlx5_4 mlx5_5 mlx5_6 mlx5_7)
PORT_BASE=${PORT_BASE:-19400}
DURATION=${DURATION:-12}
SIZE=${SIZE:-8388608}
FANOUT=${FANOUT:-8}
EXTRA_ARGS=${EXTRA_ARGS:-}

cleanup_servers() {
  ssh "$VM0" "set -e; cd '$BASE'; for f in perftest-revfan-*.pid; do [ -e \"\$f\" ] || continue; kill \$(cat \"\$f\") 2>/dev/null || true; rm -f \"\$f\"; done; rm -f perftest-revfan-*.log; echo reverse_fanout_servers_cleaned"
}

start_servers() {
  local qps=$1
  ssh "$VM0" "set -e; cd '$BASE'; rm -f perftest-revfan-*.log perftest-revfan-*.pid"
  local idx=0
  for dev in "${DEVS[@]}"; do
    for lane in $(seq 0 $((FANOUT - 1))); do
      local port=$((PORT_BASE + idx))
      ssh "$VM0" "set -e; cd '$BASE'; nohup ib_write_bw -d '$dev' -i 1 --force-link=IB --report_gbits $EXTRA_ARGS -D '$DURATION' -s '$SIZE' -q '$qps' -p '$port' > perftest-revfan-${dev}-${lane}.log 2>&1 & echo \$! > perftest-revfan-${dev}-${lane}.pid"
      idx=$((idx + 1))
    done
  done
  sleep 3
}

run_clients() {
  local qps=$1
  local devs_joined="${DEVS[*]}"
  ssh "$VM1" "set -e; cd '$BASE'; rm -f perftest-revfan-client-*.log; python3 - <<'PY'
import subprocess
devs = '$devs_joined'.split()
base = '$BASE'
port_base = $PORT_BASE
duration = '$DURATION'
size = '$SIZE'
qps = '$qps'
fanout = $FANOUT
procs = []
idx = 0
for dev in devs:
    for lane in range(fanout):
        port = str(port_base + idx)
        log = open(f'{base}/perftest-revfan-client-{dev}-{lane}.log', 'w')
        cmd = ['ib_write_bw', '-d', dev, '-i', '1', '--force-link=IB', '--report_gbits'] + '$EXTRA_ARGS'.split() + ['-D', duration, '-s', size, '-q', qps, '-p', port, '10.0.0.4']
        procs.append((dev, lane, subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT), log))
        idx += 1
failed = 0
for dev, lane, proc, log in procs:
    rc = proc.wait()
    log.close()
    print(f'client_{dev}_{lane}_done rc={rc}')
    if rc != 0:
        failed += 1
if failed:
    raise SystemExit(failed)
PY"
}

summarize() {
  local label=$1
  ssh "$VM1" "python3 - <<'PY'
from pathlib import Path
base = Path('$BASE')
total = 0.0
by_dev = {}
missing = []
for path in sorted(base.glob('perftest-revfan-client-*.log')):
    text = path.read_text(errors='replace')
    val = None
    for line in text.splitlines():
        parts = line.split()
        if len(parts) >= 4 and parts[0].isdigit():
            try:
                val = float(parts[3])
            except ValueError:
                pass
    name = path.name.removeprefix('perftest-revfan-client-').removesuffix('.log')
    dev = '_'.join(name.split('-')[0].split('_')[:2])
    if val is None:
        missing.append(path.name)
        continue
    total += val
    by_dev[dev] = by_dev.get(dev, 0.0) + val
print('label=$label qps=$qps duration=$DURATION fanout=$FANOUT')
for dev in sorted(by_dev):
    print(f'{dev} total_gbps={by_dev[dev]:.2f}')
print(f'total_gbps={total:.2f}')
if missing:
    print('missing=' + ','.join(missing))
PY"
}

run_case() {
  local label=$1
  local qps=$2
  echo "=== reverse_fanout_case=$label qps=$qps fanout=$FANOUT ==="
  cleanup_servers
  start_servers "$qps"
  run_clients "$qps"
  summarize "$label"
  cleanup_servers
}

run_case reverse_fanout_qps1 1
run_case reverse_fanout_qps2 2
