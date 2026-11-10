#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Runs inside the gantry-bench DaemonSet pods (privileged, hostPID,
# hostNetwork). "nsenter -t 1 -m -u -n -p" enters the namespaces of PID 1 of
# the host PID namespace, which gives the kubelet/containerd view
# (/run/containerd/containerd.sock, /etc/containerd/certs.d, crictl, ctr).
# This also works when containerd runs inside a systemd-nspawn machine whose
# systemd is PID 1 of the pod's PID namespace.
#
# MODE:
#   prep-apply   write /etc/containerd/certs.d/<ORIGIN>/hosts.toml
#   prep-remove  remove that directory
#   clean        remove bench images and gantry leases, report disk free
#   pull         sample node stats, pull IMAGE at START_AT, print RESULT
#
# Every mode prints a final "DONE <node>" line and then sleeps, so the
# runner can collect logs before deleting the DaemonSet.
set -euo pipefail

NODE="${NODE_NAME:?NODE_NAME}"
MODE="${MODE:?MODE}"
NIC="${NIC:-eth0}"
DISK="${DISK:-nvme0n1}"
INTERVAL="${INTERVAL:-1}"

host() { nsenter -t 1 -m -u -n -p -- "$@"; }
# crictl_ hides the harmless "/etc/crictl.yaml does not exist" warnings.
crictl_() {
  host crictl --runtime-endpoint unix:///run/containerd/containerd.sock "$@" 2> >(grep -v 'crictl.yaml' >&2)
}
now_ns() { local t="${EPOCHREALTIME/./}"; printf '%s000' "${t}"; }

finish() {
  echo "DONE ${NODE}"
  exec sleep infinity
}

prep_apply() {
  local dir="/etc/containerd/certs.d/${ORIGIN:?ORIGIN}"
  host mkdir -p "${dir}"
  host sh -c "cat > '${dir}/hosts.toml.tmp' && mv '${dir}/hosts.toml.tmp' '${dir}/hosts.toml'" <<EOF
# Managed by hack/gantry-cluster-bench (prep.sh). Bench origin registry.
server = "http://${ORIGIN}"

[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
  dial_timeout = "200ms"
EOF
  echo "PREP ${NODE} applied ${dir}/hosts.toml"
  host cat "${dir}/hosts.toml"
}

prep_remove() {
  host rm -rf "/etc/containerd/certs.d/${ORIGIN:?ORIGIN}"
  echo "PREP ${NODE} removed"
}

clean() {
  local pattern="${CLEAN_PATTERN:-^${ORIGIN:?ORIGIN}/bench/}" ids n=0 id
  ids="$(crictl_ images --no-trunc | awk -v p="${pattern}" 'NR > 1 && $1 ~ p {print $3}' | sort -u)"
  for id in ${ids}; do
    crictl_ rmi "${id}" >/dev/null && n=$((n + 1))
  done
  echo "CLEAN ${NODE} images_removed=${n}"

  # Gantry pins in-flight and served blobs with leases named gantry-*
  # (expire after 60m). Dropping them lets containerd GC the blobs now.
  local leases
  leases="$(host ctr -n k8s.io leases ls -q 2>/dev/null | grep '^gantry-' || true)"
  n=0
  for id in ${leases}; do
    host ctr -n k8s.io leases rm "${id}" >/dev/null 2>&1 && n=$((n + 1))
  done
  echo "CLEAN ${NODE} leases_removed=${n}"

  # Content not referenced by any image or lease.
  host ctr -n k8s.io content prune references >/dev/null 2>&1 || echo "CLEAN ${NODE} content prune failed"

  if [[ "${DROP_CACHES:-0}" == "1" ]]; then
    sync
    if echo 3 >/proc/sys/vm/drop_caches 2>/dev/null; then
      echo "CLEAN ${NODE} drop_caches=ok"
    else
      echo "CLEAN ${NODE} drop_caches=failed"
    fi
  fi

  local left
  left="$(crictl_ images --no-trunc | awk -v p="${pattern}" 'NR > 1 && $1 ~ p' | wc -l)"
  echo "CLEAN ${NODE} remaining_bench_images=${left} lease_count=$(host ctr -n k8s.io leases ls -q | wc -l)"
  echo "DISK ${NODE} $(host df -B1 --output=avail,size /var/lib/containerd | tail -1)"
}

# find_pid prints the PID of the first process whose comm equals $1.
find_pid() {
  local d c
  for d in /proc/[0-9]*; do
    c=""
    read -r c <"${d}/comm" 2>/dev/null || continue
    if [[ "${c}" == "$1" ]]; then
      printf '%s\n' "${d#/proc/}"
      return 0
    fi
  done
  return 1
}

# proc_stats prints "ticks rss_kb" for PID $1, or "- -".
proc_stats() {
  local pid="$1" stat rest f rss="" line
  if [[ -z "${pid}" ]] || ! read -r stat <"/proc/${pid}/stat" 2>/dev/null; then
    printf -- '- -'
    return
  fi
  rest="${stat##*) }"
  read -ra f <<<"${rest}"
  while read -r line; do
    if [[ "${line}" == VmRSS:* ]]; then
      read -r _ rss _ <<<"${line}"
      break
    fi
  done <"/proc/${pid}/status"
  # After "comm) ", utime and stime are fields 12 and 13 (0-based 11, 12).
  printf '%s %s' "$((f[11] + f[12]))" "${rss:--}"
}

# cg_stats prints "current anon file active_file inactive_file file_dirty
# file_writeback" in bytes for cgroup v2 directory $1, or dashes. The pod
# sees the host cgroup namespace (privileged, hostPID).
cg_stats() {
  local dir="$1" cur="" k v anon=- file=- af=- inf=- dirty=- wb=-
  if [[ -z "${dir}" ]] || ! read -r cur <"${dir}/memory.current" 2>/dev/null; then
    printf -- '- - - - - - -'
    return
  fi
  while read -r k v; do
    case "${k}" in
      anon) anon="${v}" ;;
      file) file="${v}" ;;
      active_file) af="${v}" ;;
      inactive_file) inf="${v}" ;;
      file_dirty) dirty="${v}" ;;
      file_writeback) wb="${v}" ;;
    esac
  done <"${dir}/memory.stat"
  printf '%s %s %s %s %s %s %s' "${cur}" "${anon}" "${file}" "${af}" "${inf}" "${dirty}" "${wb}"
}

# io_stats prints "cg_rbytes cg_wbytes proc_read_bytes proc_write_bytes"
# for cgroup v2 directory $1 (io.stat summed over devices) and process $2
# (/proc/<pid>/io), with dashes for what is unreadable.
io_stats() {
  local dir="$1" pid="$2" line kv rb=0 wb=0 cg="- -" pr=- pw=- k v
  if [[ -n "${dir}" && -r "${dir}/io.stat" ]]; then
    while read -r line; do
      for kv in ${line}; do
        case "${kv}" in
          rbytes=*) rb=$((rb + ${kv#rbytes=})) ;;
          wbytes=*) wb=$((wb + ${kv#wbytes=})) ;;
        esac
      done
    done <"${dir}/io.stat"
    cg="${rb} ${wb}"
  fi
  if [[ -n "${pid}" && -r "/proc/${pid}/io" ]]; then
    while read -r k v; do
      case "${k}" in
        read_bytes:) pr="${v}" ;;
        write_bytes:) pw="${v}" ;;
      esac
    done <"/proc/${pid}/io"
  fi
  printf '%s %s %s' "${cg}" "${pr}" "${pw}"
}

sampler() {
  local gpid="" cpid="" line rx tx dw dr busy total memt mema a gcg="" gcgpid=""
  while :; do
    [[ -n "${gpid}" && -d "/proc/${gpid}" ]] || gpid="$(find_pid gantry || true)"
    if [[ -n "${gpid}" && "${gcgpid}" != "${gpid}" ]]; then
      gcg=""
      while read -r line; do
        if [[ "${line}" == 0::* ]]; then gcg="/sys/fs/cgroup${line#0::}"; break; fi
      done <"/proc/${gpid}/cgroup" 2>/dev/null || true
      gcgpid="${gpid}"
    fi
    [[ -n "${cpid}" && -d "/proc/${cpid}" ]] || cpid="$(find_pid containerd || true)"

    rx=- tx=-
    while read -r line; do
      line="${line/:/ }"
      read -ra a <<<"${line}"
      if [[ "${a[0]}" == "${NIC}" ]]; then rx="${a[1]}" tx="${a[9]}"; break; fi
    done </proc/net/dev

    dw=- dr=-
    while read -r line; do
      read -ra a <<<"${line}"
      if [[ "${a[2]}" == "${DISK}" ]]; then dr="${a[5]}" dw="${a[9]}"; break; fi
    done </proc/diskstats

    read -ra a </proc/stat
    total=$((a[1] + a[2] + a[3] + a[4] + a[5] + a[6] + a[7] + a[8]))
    busy=$((total - a[4] - a[5]))

    memt=- mema=-
    while read -r line; do
      case "${line}" in
        MemTotal:*) read -r _ memt _ <<<"${line}" ;;
        MemAvailable:*) read -r _ mema _ <<<"${line}"; break ;;
      esac
    done </proc/meminfo

    echo "SAMPLE ${NODE} $(now_ns) ${rx} ${tx} ${dw} ${busy} ${total} ${memt} ${mema} $(proc_stats "${gpid}") $(proc_stats "${cpid}")"
    [[ -n "${gpid}" ]] || gcg=""
    echo "MEMSAMPLE ${NODE} $(now_ns) ${dr} $(cg_stats "${gcg}")"
    echo "IOSAMPLE ${NODE} $(now_ns) $(io_stats "${gcg}" "${gpid}")"
    sleep "${INTERVAL}"
  done
}

pull() {
  : "${IMAGE:?IMAGE}" "${START_AT:?START_AT}"
  sampler &
  local spid=$! start end rc=0 wait_s
  echo "INFO ${NODE} nic=${NIC} disk=${DISK} image=${IMAGE} start_at=${START_AT}"

  wait_s=$((START_AT - EPOCHSECONDS))
  if ((wait_s > 0)); then
    # Sleep to just before START_AT, then spin on the sub-second remainder.
    sleep "$((wait_s - 1))"
    while (($(now_ns) < START_AT * 1000000000)); do sleep 0.01; done
  fi

  start="$(now_ns)"
  crictl_ --timeout 30s pull --pull-timeout "${PULL_TIMEOUT:-3600}s" "${IMAGE}" >/tmp/pull.out 2>&1 || rc=$?
  end="$(now_ns)"
  echo "RESULT ${NODE} ${start} ${end} ${rc}"
  sed "s/^/PULLOUT ${NODE} /" /tmp/pull.out

  sleep "${TAIL_SECONDS:-15}"
  kill "${spid}" 2>/dev/null || true
  echo "DISK ${NODE} $(host df -B1 --output=avail,size /var/lib/containerd | tail -1)"
}

case "${MODE}" in
  prep-apply) prep_apply ;;
  prep-remove) prep_remove ;;
  clean) clean ;;
  pull) pull ;;
  sample) sampler ;;
  *) echo "unknown MODE ${MODE}" >&2; exit 2 ;;
esac

finish
