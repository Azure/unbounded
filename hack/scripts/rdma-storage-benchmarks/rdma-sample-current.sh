#!/usr/bin/env bash
set -euo pipefail

VM0=${VM0:-azureuser@20.120.39.72}
VM1=${VM1:-azureuser@172.173.244.140}
INTERVAL=${INTERVAL:-20}

sample_target() {
  ssh "$VM1" "INTERVAL=$INTERVAL python3 -" <<'PY'
import os
import pathlib
import re
import time
import urllib.request

interval = int(os.environ.get("INTERVAL", "20"))
base = pathlib.Path.home() / "ub-rdma-bench"
pid = int((base / "target.pid").read_text().strip())
ticks = os.sysconf(os.sysconf_names["SC_CLK_TCK"])

metric_re = re.compile(
    r'^(unbounded_storage_(fabric_bytes_written_total|fabric_pages_written_total|fabric_connections|fabric_rpc_inflight|fabric_rpc_served_total|p2p_requests_total))(\{[^}]*\})?\s+([0-9.eE+-]+)$'
)

def metrics():
    text = urllib.request.urlopen("http://127.0.0.1:9100/metrics", timeout=5).read().decode()
    out = {}
    for line in text.splitlines():
        m = metric_re.match(line)
        if not m:
            continue
        name = m.group(1) + (m.group(3) or "")
        out[name] = float(m.group(4))
    return out

def task_cpu():
    data = {}
    task_dir = pathlib.Path(f"/proc/{pid}/task")
    for task in task_dir.iterdir():
        try:
            stat = (task / "stat").read_text()
            name = stat.split(")", 1)[0].split("(", 1)[1]
            rest = stat.split(")", 1)[1].split()
            cpu_ticks = int(rest[11]) + int(rest[12])
        except Exception:
            continue
        data[int(task.name)] = (name, cpu_ticks)
    return data

def hca_xmit():
    out = {}
    root = pathlib.Path("/sys/class/infiniband")
    for hca in sorted(root.glob("mlx5_*")):
        p = hca / "ports/1/counters/port_xmit_data"
        try:
            out[hca.name] = int(p.read_text())
        except Exception:
            pass
    return out

m0 = metrics()
c0 = task_cpu()
h0 = hca_xmit()
time.sleep(interval)
m1 = metrics()
c1 = task_cpu()
h1 = hca_xmit()

print(f"host=vm1_target pid={pid} interval_seconds={interval}")
for key in sorted(set(m0) | set(m1)):
    start = m0.get(key, 0.0)
    end = m1.get(key, 0.0)
    delta = end - start
    if key.startswith("unbounded_storage_fabric_bytes_written_total"):
        print(f"{key} start={start:.0f} end={end:.0f} delta={delta:.0f} throughput_gbps={delta * 8 / interval / 1e9:.3f}")
    else:
        print(f"{key} start={start:.0f} end={end:.0f} delta={delta:.0f}")

by_name = {}
for tid, (name, ticks1) in c1.items():
    if tid not in c0:
        continue
    ticks0 = c0[tid][1]
    by_name[name] = by_name.get(name, 0.0) + (ticks1 - ticks0) / ticks / interval * 100.0
for name, pcpu in sorted(by_name.items(), key=lambda item: -item[1])[:12]:
    print(f"cpu_by_thread_name name={name} pcpu_sum={pcpu:.1f}")

total = 0.0
for hca in sorted(set(h0) | set(h1)):
    delta = h1.get(hca, 0) - h0.get(hca, 0)
    gbps = delta * 4 * 8 / interval / 1e9
    total += gbps
    print(f"hca_xmit {hca} gbps={gbps:.3f}")
print(f"hca_xmit_total_gbps={total:.3f}")
PY
}

sample_source() {
  ssh "$VM0" "INTERVAL=$INTERVAL python3 -" <<'PY'
import os
import pathlib
import re
import time
import urllib.request

interval = int(os.environ.get("INTERVAL", "20"))
base = pathlib.Path.home() / "ub-rdma-bench"
pid = int((base / "source.pid").read_text().strip())
ticks = os.sysconf(os.sysconf_names["SC_CLK_TCK"])

metric_re = re.compile(
    r'^(unbounded_storage_(frontend_response_bytes_total|frontend_requests_total|fabric_connections|fabric_bytes_written_total|fabric_pages_written_total))(\{[^}]*\})?\s+([0-9.eE+-]+)$'
)

def metrics():
    text = urllib.request.urlopen("http://127.0.0.1:9100/metrics", timeout=5).read().decode()
    out = {}
    for line in text.splitlines():
        m = metric_re.match(line)
        if not m:
            continue
        name = m.group(1) + (m.group(3) or "")
        out[name] = float(m.group(4))
    return out

def task_cpu():
    data = {}
    task_dir = pathlib.Path(f"/proc/{pid}/task")
    for task in task_dir.iterdir():
        try:
            stat = (task / "stat").read_text()
            name = stat.split(")", 1)[0].split("(", 1)[1]
            rest = stat.split(")", 1)[1].split()
            cpu_ticks = int(rest[11]) + int(rest[12])
        except Exception:
            continue
        data[int(task.name)] = (name, cpu_ticks)
    return data

def hca_rcv():
    out = {}
    root = pathlib.Path("/sys/class/infiniband")
    for hca in sorted(root.glob("mlx5_*")):
        p = hca / "ports/1/counters/port_rcv_data"
        try:
            out[hca.name] = int(p.read_text())
        except Exception:
            pass
    return out

m0 = metrics()
c0 = task_cpu()
h0 = hca_rcv()
time.sleep(interval)
m1 = metrics()
c1 = task_cpu()
h1 = hca_rcv()

print(f"host=vm0_source pid={pid} interval_seconds={interval}")
for key in sorted(set(m0) | set(m1)):
    start = m0.get(key, 0.0)
    end = m1.get(key, 0.0)
    delta = end - start
    if key.startswith("unbounded_storage_frontend_response_bytes_total") or key.startswith("unbounded_storage_fabric_bytes_written_total"):
        print(f"{key} start={start:.0f} end={end:.0f} delta={delta:.0f} throughput_gbps={delta * 8 / interval / 1e9:.3f}")
    else:
        print(f"{key} start={start:.0f} end={end:.0f} delta={delta:.0f}")

by_name = {}
for tid, (name, ticks1) in c1.items():
    if tid not in c0:
        continue
    ticks0 = c0[tid][1]
    by_name[name] = by_name.get(name, 0.0) + (ticks1 - ticks0) / ticks / interval * 100.0
for name, pcpu in sorted(by_name.items(), key=lambda item: -item[1])[:12]:
    print(f"cpu_by_thread_name name={name} pcpu_sum={pcpu:.1f}")

total = 0.0
for hca in sorted(set(h0) | set(h1)):
    delta = h1.get(hca, 0) - h0.get(hca, 0)
    gbps = delta * 4 * 8 / interval / 1e9
    total += gbps
    print(f"hca_rcv {hca} gbps={gbps:.3f}")
print(f"hca_rcv_total_gbps={total:.3f}")
PY
}

sample_target > /tmp/rdma-sample-target.out &
target_pid=$!
sample_source > /tmp/rdma-sample-source.out &
source_pid=$!
wait "$target_pid"
wait "$source_pid"
cat /tmp/rdma-sample-target.out
cat /tmp/rdma-sample-source.out
