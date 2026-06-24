#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/rdma-storage-noverify-qps.sh"

PEERS=(2672764 4903914 3252266 4399901 3987422 786476 3276438 2516518)
ROUTING_PREDECESSOR=1770102

write_source_config_scoped() {
  local workers=$1
  local bytes=$2
  shift 2
  local addrs=("$@")
  local stripe_bytes=${STRIPE_BYTES:-1048576}
  local progress_threads=${PROGRESS_THREADS:-2}
  local rpc_worker_threads=${RPC_WORKER_THREADS:-16}
  local nic_workers=${NIC_WORKERS:-2}
  local predecessor=${ROUTING_PREDECESSOR:-${PEERS[7]}}
  local fingers
  fingers=$(IFS=,; echo "${PEERS[*]}")
  ssh "$VM0" "cat > '$BASE/rdma-source.toml'" <<EOF_SRC
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
EOF_SRC
  for i in "${!PEERS[@]}"; do
    cat <<EOF_PEER | ssh "$VM0" "cat >> '$BASE/rdma-source.toml'"
[[neighborhoods.peers]]
id = ${PEERS[$i]}
tags = ["unit=$i"]
[neighborhoods.peers.config.rdma]
addr = "${addrs[$i]}"
EOF_PEER
  done
  cat <<EOF_FRONTEND | ssh "$VM0" "cat >> '$BASE/rdma-source.toml'"

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
EOF_FRONTEND
}

start_source() {
  ssh "$VM0" 'set -e; cd "$HOME/ub-rdma-bench"; if [ -f source.pid ]; then sudo kill $(cat source.pid) 2>/dev/null || true; rm -f source.pid; fi; rm -f source.log; sudo env LD_LIBRARY_PATH=/home/azureuser/ub-rdma-bench/unbounded-storage-linux-amd64/lib RUST_BACKTRACE=1 UNBOUNDED_STORAGE_LOG=warn bash -c "ulimit -l unlimited; nohup /home/azureuser/ub-rdma-bench/unbounded-storage-linux-amd64/bin/unbounded-storage --config /home/azureuser/ub-rdma-bench/rdma-source.toml > /home/azureuser/ub-rdma-bench/source.log 2>&1 & echo \$! > /home/azureuser/ub-rdma-bench/source.pid"; sleep 8; if sudo kill -0 $(cat source.pid) 2>/dev/null; then echo source_running=$(cat source.pid); else echo source_exited; fi; grep "fabric unit up" source.log || true; printf "source_errors_initial="; grep -c "outcome=err" source.log || true'
}

run_case_scoped() {
  local name=$1
  local workers=$2
  local bytes=$3
  echo "case=$name workers=$workers read_bytes=$bytes qps=1(default) pipe=8(default) verify=false scoped_source_peers=true"
  stop_daemon "$VM0" source
  stop_daemon "$VM1" target
  write_target_config "$bytes"
  start_target
  mapfile -t entries < <(target_addrs)
  printf 'target_addrs=%s\n' "${entries[*]}"
  if [ "${#entries[@]}" -ne 8 ]; then
    echo "expected 8 target addresses, got ${#entries[@]}" >&2
    exit 1
  fi
  local dev_order=(mlx5_2 mlx5_0 mlx5_6 mlx5_4 mlx5_3 mlx5_1 mlx5_7 mlx5_5)
  local addrs=()
  for dev in "${dev_order[@]}"; do
    local found=""
    for entry in "${entries[@]}"; do
      if [[ "$entry" == "$dev="* ]]; then
        found=${entry#*=}
      fi
    done
    if [ -z "$found" ]; then
      echo "missing target address for $dev" >&2
      exit 1
    fi
    addrs+=("$found")
  done
  write_source_config_scoped "$workers" "$bytes" "${addrs[@]}"
  start_source
  measure_host "$VM0" vm0_source
  measure_host "$VM1" vm1_target
  measure_target_hcas
  check_errors
}

main() {
  run_case_scoped scoped_marker_w64_16m 64 16777216
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main
fi
