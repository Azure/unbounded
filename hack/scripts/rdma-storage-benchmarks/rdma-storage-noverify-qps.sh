#!/usr/bin/env bash
set -euo pipefail

VM0="azureuser@20.120.39.72"
VM1="azureuser@172.173.244.140"
BASE="/home/azureuser/ub-rdma-bench"
BIN="$BASE/unbounded-storage-linux-amd64/bin/unbounded-storage"
LIB="$BASE/unbounded-storage-linux-amd64/lib"

PEERS=(4903914 3252266 4399901 3987422 786476 3276438 2516518 1770102)

stop_daemon() {
  local host=$1
  local name=$2
  ssh "$host" "set -e; cd '$BASE'; if [ -f ${name}.pid ]; then sudo kill \$(cat ${name}.pid) 2>/dev/null || true; sleep 1; rm -f ${name}.pid; fi; rm -f ${name}.log; echo ${name}_stopped"
}

write_target_config() {
  local bytes=$1
  local stripe_bytes=${STRIPE_BYTES:-1048576}
  local progress_threads=${PROGRESS_THREADS:-2}
  local rpc_worker_threads=${RPC_WORKER_THREADS:-16}
  local nic_workers=${NIC_WORKERS:-2}
  ssh "$VM1" "cat > '$BASE/rdma-target.toml'" <<TOML
[startup.memory]
memory_total_bytes = 2147483648

[startup.metrics]
addr = "0.0.0.0:9100"

[startup.fabric]
max_inflight = 16384
progress_threads = $progress_threads
rpc_worker_threads = $rpc_worker_threads
progress_poll_us = 0

[startup.fabric.binds.auto_rdma]
hcas_per_numa_node = 2

[startup.topology]
serving_cores = 32
nic_workers = $nic_workers

[[backends]]
name = "bench-backend"
[backends.config.fake]
stripe_size_bytes = $stripe_bytes
object_size_bytes = $bytes

[[neighborhoods]]
name = "bench-neighborhood"
source = "bench-backend"
local_node_id = 2
TOML
}

write_source_config() {
  local workers=$1
  local bytes=$2
  shift 2
  local addrs=("$@")
  local fingers
  local predecessor=${ROUTING_PREDECESSOR:-${PEERS[7]}}
  local stripe_bytes=${STRIPE_BYTES:-1048576}
  local progress_threads=${PROGRESS_THREADS:-2}
  local rpc_worker_threads=${RPC_WORKER_THREADS:-16}
  local nic_workers=${NIC_WORKERS:-2}
  fingers=$(printf '%s, ' "${PEERS[@]}")
  fingers="${fingers%, }"
  ssh "$VM0" "cat > '$BASE/rdma-source.toml'" <<TOML
[startup.memory]
memory_total_bytes = 2147483648

[startup.metrics]
addr = "0.0.0.0:9100"

[startup.fabric]
max_inflight = 16384
progress_threads = $progress_threads
rpc_worker_threads = $rpc_worker_threads
progress_poll_us = 0

[startup.fabric.binds.auto_rdma]
hcas_per_numa_node = 2

[startup.topology]
serving_cores = 32
nic_workers = $nic_workers

[[backends]]
name = "bench-backend"
[backends.config.fake]
stripe_size_bytes = $stripe_bytes
object_size_bytes = $bytes

[[neighborhoods]]
name = "bench-neighborhood"
source = "bench-backend"
local_node_id = 1

[neighborhoods.routing_plan]
fingers = [$fingers]
successor = ${PEERS[0]}
predecessor = $predecessor
TOML
  for i in "${!PEERS[@]}"; do
    ssh "$VM0" "cat >> '$BASE/rdma-source.toml'" <<TOML

[[neighborhoods.peers]]
id = ${PEERS[$i]}
[neighborhoods.peers.config.rdma]
addr = "${addrs[$i]}"
TOML
  done
  ssh "$VM0" "cat >> '$BASE/rdma-source.toml'" <<TOML

[[frontends]]
name = "rdma-loadgen"
source = "bench-neighborhood"
[frontends.config.loadgen]
workers = $workers
seed = 3203336958
object_count = 1000000
read_bytes = $bytes
verify = false
fixed_object_size_bytes = $bytes
require_remote_peer = true
warmup_operations = 1000
TOML
}

start_target() {
  ssh "$VM1" "set -e; cd '$BASE'; rm -f target.log target.pid; sudo env LD_LIBRARY_PATH='$LIB' RUST_BACKTRACE=1 UNBOUNDED_STORAGE_LOG=warn bash -c 'ulimit -l unlimited; nohup $BIN --config $BASE/rdma-target.toml > $BASE/target.log 2>&1 & echo \$! > $BASE/target.pid'; sleep 8; if sudo kill -0 \$(cat target.pid) 2>/dev/null; then echo target_running=\$(cat target.pid); else echo target_exited; fi; grep 'fabric unit up' target.log"
}

target_addrs() {
  ssh "$VM1" "python3 - <<'PY'
from pathlib import Path
for line in Path('$BASE/target.log').read_text().splitlines():
    if 'fabric unit up:' not in line:
        continue
    dev = line.split(' dev=', 1)[1].split(' ', 1)[0]
    addr = line.split(' self_addr=', 1)[1].split(' self_addr_native=', 1)[0]
    print(f'{dev}={addr}')
PY"
}

start_source() {
  ssh "$VM0" "set -e; cd '$BASE'; rm -f source.log source.pid; sudo env LD_LIBRARY_PATH='$LIB' RUST_BACKTRACE=1 UNBOUNDED_STORAGE_LOG=warn bash -c 'ulimit -l unlimited; nohup $BIN --config $BASE/rdma-source.toml > $BASE/source.log 2>&1 & echo \$! > $BASE/source.pid'; sleep 20; if sudo kill -0 \$(cat source.pid) 2>/dev/null; then echo source_running=\$(cat source.pid); else echo source_exited; fi; grep 'fabric unit up' source.log; grep -c 'outcome=err' source.log | sed 's/^/source_errors_initial=/'"
}

measure_host() {
  local host=$1
  local label=$2
  ssh "$host" "python3 - <<'PY'
import time, urllib.request
keys = [
    'unbounded_storage_frontend_response_bytes_total',
    'unbounded_storage_frontend_requests_total',
    'unbounded_storage_fabric_bytes_written_total',
    'unbounded_storage_fabric_pages_written_total',
    'unbounded_storage_fabric_connections',
    'unbounded_storage_p2p_requests_total',
]
def scrape():
    text = urllib.request.urlopen('http://127.0.0.1:9100/metrics', timeout=5).read().decode()
    out = {}
    for line in text.splitlines():
        if line.startswith('#') or not line.strip():
            continue
        name = line.split('{', 1)[0].split(None, 1)[0]
        if name not in keys:
            continue
        out[line.rsplit(None, 1)[0]] = float(line.rsplit(None, 1)[1])
    return out
start = scrape(); time.sleep(20); end = scrape()
print('host=$label interval_seconds=20')
for key in sorted(set(start) | set(end)):
    s = start.get(key, 0.0); e = end.get(key, 0.0); d = e - s
    print(f'{key} start={s:.0f} end={e:.0f} delta={d:.0f}')
    if 'bytes' in key:
        print(f'{key} throughput_gbps={d * 8 / 20 / 1e9:.3f}')
PY"
}

measure_target_hcas() {
  ssh "$VM1" "python3 - <<'PY'
import time
from pathlib import Path
hcadir = Path('/sys/class/infiniband')
def sample():
    return {h.name: int((h / 'ports/1/counters/port_xmit_data').read_text().strip()) for h in sorted(hcadir.glob('mlx5_*'))}
start = sample(); time.sleep(10); end = sample()
print('host=vm1_target ib_counter_interval_seconds=10')
total = 0.0
for h in sorted(end):
    gbps = (end[h] - start[h]) * 4 * 8 / 10 / 1e9
    total += gbps
    print(f'{h} xmit_gbps={gbps:.3f}')
print(f'total_xmit_gbps={total:.3f}')
PY"
}

check_errors() {
  ssh "$VM0" "set -e; cd '$BASE'; printf 'source_errors='; grep -c 'outcome=err' source.log || true; printf 'source_no_scratch='; grep -c 'no scratch page available' source.log || true; printf 'source_log_lines='; wc -l < source.log"
  ssh "$VM1" "set -e; cd '$BASE'; printf 'target_errors='; grep -c 'outcome=err' target.log || true; printf 'target_no_scratch='; grep -c 'no scratch page available' target.log || true; printf 'target_log_lines='; wc -l < target.log"
}

run_case() {
  local name=$1
  local workers=$2
  local bytes=$3
  echo "=== storage_case=$name workers=$workers read_bytes=$bytes qps=1(default) pipe=8(default) verify=false ==="
  stop_daemon "$VM0" source
  stop_daemon "$VM1" target
  write_target_config "$bytes"
  start_target
  mapfile -t entries < <(target_addrs)
  printf '%s\n' "${entries[@]}"
  if [[ ${#entries[@]} -ne 8 ]]; then
    echo "expected 8 target addresses, got ${#entries[@]}" >&2
    exit 1
  fi
  local addrs=()
  local devs=(mlx5_2 mlx5_0 mlx5_6 mlx5_4 mlx5_3 mlx5_1 mlx5_7 mlx5_5)
  for dev in "${devs[@]}"; do
    local found=""
    for entry in "${entries[@]}"; do
      if [[ "$entry" == "$dev="* ]]; then
        found=${entry#*=}
      fi
    done
    if [[ -z "$found" ]]; then
      echo "missing target address for $dev" >&2
      exit 1
    fi
    addrs+=("$found")
  done
  write_source_config "$workers" "$bytes" "${addrs[@]}"
  start_source
  measure_host "$VM0" vm0_source
  measure_host "$VM1" vm1_target
  measure_target_hcas
  check_errors
}

main() {
  run_case noverify_w64_1m 64 1048576
  run_case noverify_w16_8m 16 8388608
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main
fi
