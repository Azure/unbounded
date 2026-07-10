#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORK_DIR="${ROOT}/tmp/playpen-e2e"
CLUSTER_NAME="${PLAYPEN_E2E_CLUSTER:-playpen-e2e}"
CONTEXT="kind-${CLUSTER_NAME}"
IMAGE="playpen:e2e"
NAMESPACE="playpen-e2e"
CLIENT_NS="playpen-e2e-a"
SECOND_CLIENT_NS="playpen-e2e-b"
CLIENT_CIDR="172.30.11.2/30"
CLIENT_GATEWAY="172.30.11.1"
SECOND_CLIENT_CIDR="172.30.12.2/30"
SECOND_CLIENT_GATEWAY="172.30.12.1"
PXE_CIDR="192.168.100.1/24"
ALPINE_VERSION="${ALPINE_VERSION:-3.22.1}"
ALPINE_BRANCH="v${ALPINE_VERSION%.*}"
ALPINE_BASE="https://dl-cdn.alpinelinux.org/alpine/${ALPINE_BRANCH}/releases/x86_64/netboot"
MARKER="playpen_e2e=alpine-pxe-ok"
BMC_USERNAME="admin"
BMC_PASSWORD="playpen-e2e"
BMC_DEVICE_ID="1"
CLIENT_PID=""
SECOND_CLIENT_PID=""
POD_IP=""
NODE_IP=""
CLAIMED_MAC=""

log() {
    printf 'playpen-e2e: %s\n' "$*"
}

fail() {
    log "$*"
    exit 1
}

wait_for() {
    local description=$1
    local timeout=$2
    shift 2

    local deadline=$((SECONDS + timeout))
    while (( SECONDS < deadline )); do
        if "$@"; then
            return 0
        fi
        sleep 1
    done

    log "timed out waiting for ${description}"
    return 1
}

redfish() {
    local method=$1
    local path=$2
    local body=${3:-}
    local args=(
        --fail-with-body --silent --show-error --insecure
        --user "${BMC_USERNAME}:${BMC_PASSWORD}"
        --request "${method}"
        "https://${POD_IP}:8443${path}"
    )

    if [[ -n "${body}" ]]; then
        args+=(--header 'Content-Type: application/json' --data "${body}")
    fi

    curl "${args[@]}"
}

power_is() {
    local expected=$1
    [[ $(redfish GET "/redfish/v1/Systems/${BMC_DEVICE_ID}" | jq -r .PowerState) == "${expected}" ]]
}

bmc_fingerprint() {
    openssl s_client -connect "${POD_IP}:8443" </dev/null 2>/dev/null |
        openssl x509 -noout -fingerprint -sha256
}

pod_exec() {
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec playpen-0 -- "$@"
}

dump_diagnostics() {
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pods,pvc -o wide || true
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" describe pod playpen-0 || true
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs playpen-0 >"${WORK_DIR}/playpen.log" 2>&1 || true
    if [[ -f "${WORK_DIR}/dnsmasq.log" ]]; then
        printf '%s\n' '--- dnsmasq.log ---'
        tail -n 100 "${WORK_DIR}/dnsmasq.log" || true
    fi
    if [[ -f "${WORK_DIR}/playpen.log" ]]; then
        printf '%s\n' '--- playpen.log ---'
        tail -n 200 "${WORK_DIR}/playpen.log" || true
    fi
}

cleanup() {
    set +e
    if [[ -n "${SECOND_CLIENT_PID}" ]]; then
        sudo kill "${SECOND_CLIENT_PID}" 2>/dev/null
        wait "${SECOND_CLIENT_PID}" 2>/dev/null
    fi
    if [[ -n "${CLIENT_PID}" ]]; then
        sudo kill "${CLIENT_PID}" 2>/dev/null
        wait "${CLIENT_PID}" 2>/dev/null
    fi
    sudo ip netns delete "${SECOND_CLIENT_NS}" 2>/dev/null
    sudo ip netns delete "${CLIENT_NS}" 2>/dev/null
    sudo iptables -D FORWARD -d 172.30.0.0/16 -j ACCEPT 2>/dev/null
    sudo iptables -D FORWARD -s 172.30.0.0/16 -j ACCEPT 2>/dev/null
    if [[ -n "${POD_IP}" ]]; then
        sudo ip route delete "${POD_IP}/32" via "${NODE_IP}" 2>/dev/null
    fi
    if [[ "${KEEP_PLAYPEN_E2E_CLUSTER:-0}" != "1" ]]; then
        kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1
    fi
}
trap cleanup EXIT

for command in docker kind kubectl curl dnsmasq jq openssl sudo; do
    command -v "${command}" >/dev/null || { log "missing required command: ${command}"; exit 1; }
done
[[ -c /dev/kvm ]] || { log "/dev/kvm is required"; exit 1; }
sudo -n true || { log "passwordless sudo is required"; exit 1; }

mkdir -p "${WORK_DIR}/tftp/grub"
cat >"${WORK_DIR}/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
EOF

log "downloading Alpine ${ALPINE_VERSION} netboot files"
curl -fsSL --retry 3 -o "${WORK_DIR}/tftp/vmlinuz-lts" "${ALPINE_BASE}/vmlinuz-lts"
curl -fsSL --retry 3 -o "${WORK_DIR}/tftp/initramfs-lts" "${ALPINE_BASE}/initramfs-lts"
cp /usr/lib/grub/x86_64-efi/monolithic/grubnetx64.efi "${WORK_DIR}/tftp/bootx64.efi"
BOOT_FILE_BLOCKS=$((($(stat -c %s "${WORK_DIR}/tftp/bootx64.efi") + 511) / 512))
cat >"${WORK_DIR}/tftp/grub/grub.cfg" <<EOF
serial --unit=0 --speed=115200
terminal_input serial
terminal_output serial
set timeout=0
set default=0
menuentry 'Alpine playpen e2e' {
    linux /vmlinuz-lts console=ttyS0,115200 ${MARKER} modules=loop,squashfs,sd-mod,usb-storage,tpm,tpm_tis,tpm_crb
    initrd /initramfs-lts
}
EOF

log "building binaries and container image"
go build -o "${ROOT}/bin/playpen" "${ROOT}/cmd/playpen"
docker build -t "${IMAGE}" -f "${ROOT}/images/playpen/Containerfile" "${ROOT}"

kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
log "creating kind cluster ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}" --config "${WORK_DIR}/kind.yaml" --wait 120s
kind load docker-image --name "${CLUSTER_NAME}" "${IMAGE}"
kubectl --context "${CONTEXT}" apply --dry-run=client -f "${ROOT}/deploy/playpen/rendered" >/dev/null

NODE_CONTAINER="${CLUSTER_NAME}-control-plane"
NODE_IP=$(docker inspect "${NODE_CONTAINER}" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
HOST_GATEWAY=$(docker inspect "${NODE_CONTAINER}" --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}')
[[ -n "${NODE_IP}" && -n "${HOST_GATEWAY}" ]] || { log "could not detect kind underlay addresses"; exit 1; }

log "enabling L3 forwarding between kind and the client endpoint"
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo iptables -I FORWARD -d 172.30.0.0/16 -j ACCEPT
sudo iptables -I FORWARD -s 172.30.0.0/16 -j ACCEPT
docker exec "${NODE_CONTAINER}" ip route replace 172.30.11.2/32 via "${HOST_GATEWAY}"

log "deploying playpen"
kubectl --context "${CONTEXT}" create namespace "${NAMESPACE}"
cat >"${WORK_DIR}/playpen.yaml" <<EOF
apiVersion: v1
kind: Service
metadata:
  name: playpen
  namespace: ${NAMESPACE}
spec:
  clusterIP: None
  selector:
    app.kubernetes.io/name: playpen
  ports:
  - name: redfish
    port: 8443
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: playpen
  namespace: ${NAMESPACE}
spec:
  serviceName: playpen
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: playpen
  template:
    metadata:
      labels:
        app.kubernetes.io/name: playpen
    spec:
      containers:
      - name: playpen
        image: ${IMAGE}
        imagePullPolicy: Never
        securityContext:
          privileged: true
        args:
        - server
        - --vxlan-remote=172.30.11.2
        - --vxlan-vni=101
        - --mac-identity=\$(PLAYPEN_POD_NAMESPACE)/\$(PLAYPEN_POD_NAME)
        - --cpus=1
        - --memory=512M
        - --disk=/var/lib/playpen/disk.raw
        - --disk-size=64M
        - --bmc-listen=:8443
        - --bmc-username=${BMC_USERNAME}
        - --bmc-password=${BMC_PASSWORD}
        - --bmc-device-id=${BMC_DEVICE_ID}
        - --extra-qemu-arg=-no-reboot
        env:
        - name: PLAYPEN_POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: PLAYPEN_POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        volumeMounts:
        - name: kvm
          mountPath: /dev/kvm
        - name: tun
          mountPath: /dev/net/tun
        - name: playpen-state
          mountPath: /var/lib/playpen
      volumes:
      - name: kvm
        hostPath:
          path: /dev/kvm
          type: CharDevice
      - name: tun
        hostPath:
          path: /dev/net/tun
          type: CharDevice
  volumeClaimTemplates:
  - metadata:
      name: playpen-state
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 128Mi
EOF
kubectl --context "${CONTEXT}" apply -f "${WORK_DIR}/playpen.yaml"

kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status statefulset/playpen --timeout=120s
POD_IP=$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod playpen-0 -o jsonpath='{.status.podIP}')
[[ -n "${POD_IP}" ]] || { log "could not detect playpen pod IP"; exit 1; }
sudo ip route replace "${POD_IP}/32" via "${NODE_IP}"

kind get kubeconfig --name "${CLUSTER_NAME}" >"${WORK_DIR}/kubeconfig"
chmod 644 "${WORK_DIR}/kubeconfig"

log "starting isolated dnsmasq endpoint through a pod claim"
sudo "${ROOT}/bin/playpen" client \
    --namespace "${CLIENT_NS}" \
    --endpoint-cidr "${CLIENT_CIDR}" \
    --gateway-ip "${CLIENT_GATEWAY}" \
    --pod-namespace "${NAMESPACE}" \
    --pod-selector app.kubernetes.io/name=playpen \
    --kubeconfig "${WORK_DIR}/kubeconfig" \
    --bridge-cidr "${PXE_CIDR}" \
    --vxlan-vni 101 \
    -- dnsmasq --keep-in-foreground --user=root --log-dhcp --log-facility=- \
        --interface=br-playpen --bind-dynamic --port=0 \
        --dhcp-range=192.168.100.10,192.168.100.20,255.255.255.0,5m \
        --dhcp-option=3,192.168.100.1 \
        --dhcp-option=6,192.168.100.1 \
        --dhcp-option=13,"${BOOT_FILE_BLOCKS}" \
        --dhcp-boot=bootx64.efi \
        --enable-tftp --tftp-root="${WORK_DIR}/tftp" \
        --dhcp-leasefile="${WORK_DIR}/dnsmasq.leases" \
        >"${WORK_DIR}/dnsmasq.log" 2>&1 &
CLIENT_PID=$!

if ! wait_for "playpen claim" 30 grep -q '^PLAYPEN_VM_MAC=' "${WORK_DIR}/dnsmasq.log"; then
    dump_diagnostics
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod playpen-0 -o json || true
    cat "${WORK_DIR}/dnsmasq.log" || true
    exit 1
fi
CLAIMED_MAC=$(grep '^PLAYPEN_VM_MAC=' "${WORK_DIR}/dnsmasq.log" | tail -n 1 | cut -d= -f2)
[[ "${CLAIMED_MAC}" =~ ^([[:xdigit:]]{2}:){5}[[:xdigit:]]{2}$ ]] || fail "invalid claimed MAC: ${CLAIMED_MAC}"
[[ $(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod playpen-0 -o jsonpath='{.metadata.annotations.playpen\.unbounded-cloud\.io/claimed-by}') != "" ]] || fail "pod was not annotated as claimed"

log "starting a concurrent client endpoint to verify namespace isolation"
sudo "${ROOT}/bin/playpen" client \
    --namespace "${SECOND_CLIENT_NS}" \
    --endpoint-cidr "${SECOND_CLIENT_CIDR}" \
    --gateway-ip "${SECOND_CLIENT_GATEWAY}" \
    --remote "${POD_IP}" \
    --bridge-cidr 192.168.101.1/24 \
    --vxlan-vni 102 \
    -- sleep 600 >"${WORK_DIR}/second-client.log" 2>&1 &
SECOND_CLIENT_PID=$!

for _ in $(seq 1 50); do
    if sudo ip netns list | grep -q "^${CLIENT_NS}\b" && sudo ip netns list | grep -q "^${SECOND_CLIENT_NS}\b"; then
        break
    fi
    sleep 0.1
done
sudo ip netns list | grep -q "^${CLIENT_NS}\b"
sudo ip netns list | grep -q "^${SECOND_CLIENT_NS}\b"
kill -0 "${CLIENT_PID}"
kill -0 "${SECOND_CLIENT_PID}"

log "waiting for Alpine PXE boot marker"
deadline=$((SECONDS + 180))
while (( SECONDS < deadline )); do
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs playpen-0 >"${WORK_DIR}/playpen.log" 2>&1 || true
    if grep -q "${MARKER}" "${WORK_DIR}/playpen.log" && grep -qi "Alpine" "${WORK_DIR}/playpen.log"; then
        log "Alpine PXE boot succeeded"
        break
    fi
    if ! kill -0 "${CLIENT_PID}" 2>/dev/null; then
        log "dnsmasq client exited unexpectedly"
        cat "${WORK_DIR}/dnsmasq.log"
        exit 1
    fi
    sleep 2
done

grep -q "${MARKER}" "${WORK_DIR}/playpen.log" || { dump_diagnostics; fail "timed out waiting for Alpine PXE boot"; }
grep -Eqi "DHCPACK.*${CLAIMED_MAC}|${CLAIMED_MAC}.*DHCPACK" "${WORK_DIR}/dnsmasq.log" || { dump_diagnostics; fail "claimed MAC did not acquire the PXE lease"; }

log "verifying the running VM disk and TPM wiring"
pod_exec test "$(pod_exec stat -c %s /var/lib/playpen/disk.raw)" -eq 67108864
pod_exec test -S /run/playpen/swtpm.sock
pod_exec test -n "$(pod_exec find /var/lib/playpen/tpm -type f -print -quit)"
pod_exec sh -c "tr '\\0' ' ' </proc/\$(pgrep -f '[q]emu-system-x86_64' | head -n 1)/cmdline" >"${WORK_DIR}/qemu.args"
grep -q "mac=${CLAIMED_MAC}" "${WORK_DIR}/qemu.args" || fail "QEMU does not use the claimed MAC"
grep -q 'tpm-tis,tpmdev=tpm0' "${WORK_DIR}/qemu.args" || fail "QEMU TPM device is missing"
grep -q 'file=/var/lib/playpen/disk.raw' "${WORK_DIR}/qemu.args" || fail "QEMU persistent disk is missing"

log "exercising Redfish power and one-time HDD boot control"
FINGERPRINT=$(bmc_fingerprint)
[[ -n "${FINGERPRINT}" ]] || fail "could not capture the Redfish certificate fingerprint"
wait_for "initial Redfish power-on state" 30 power_is On
BOOT=$(redfish GET "/redfish/v1/Systems/${BMC_DEVICE_ID}" | jq -r '.Boot.BootSourceOverrideTarget + "/" + .Boot.BootSourceOverrideEnabled')
[[ "${BOOT}" == "Pxe/Continuous" ]] || fail "unexpected initial boot override: ${BOOT}"
redfish PATCH "/redfish/v1/Systems/${BMC_DEVICE_ID}" '{"Boot":{"BootSourceOverrideTarget":"Hdd","BootSourceOverrideEnabled":"Once"}}'
redfish POST "/redfish/v1/Systems/${BMC_DEVICE_ID}/Actions/ComputerSystem.Reset" '{"ResetType":"ForceOff"}'
wait_for "Redfish power-off state" 30 power_is Off
pod_exec sh -c "printf playpen-persistent-disk | dd of=/var/lib/playpen/disk.raw bs=1 seek=67108832 conv=notrunc status=none"
redfish POST "/redfish/v1/Systems/${BMC_DEVICE_ID}/Actions/ComputerSystem.Reset" '{"ResetType":"On"}'
wait_for "Redfish power-on state" 30 power_is On
BOOT=$(redfish GET "/redfish/v1/Systems/${BMC_DEVICE_ID}" | jq -r '.Boot.BootSourceOverrideTarget + "/" + .Boot.BootSourceOverrideEnabled')
[[ "${BOOT}" == "Hdd/Disabled" ]] || fail "one-time HDD override was not consumed: ${BOOT}"
pod_exec sh -c "tr '\\0' ' ' </proc/\$(pgrep -f '[q]emu-system-x86_64' | head -n 1)/cmdline" >"${WORK_DIR}/qemu-hdd.args"
grep -q 'virtio-blk-pci,drive=disk0,bootindex=1' "${WORK_DIR}/qemu-hdd.args" || fail "HDD was not first in the one-time boot order"
grep -q 'virtio-net-pci.*bootindex=2' "${WORK_DIR}/qemu-hdd.args" || fail "PXE was not second in the one-time boot order"

log "recreating the pod to verify persistent machine identity and state"
redfish POST "/redfish/v1/Systems/${BMC_DEVICE_ID}/Actions/ComputerSystem.Reset" '{"ResetType":"ForceOff"}'
wait_for "Redfish power-off state before recreation" 30 power_is Off
OLD_UID=$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod playpen-0 -o jsonpath='{.metadata.uid}')
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" delete pod playpen-0 --wait=false
wait_for "replacement playpen pod" 120 sh -c "uid=\$(kubectl --context '${CONTEXT}' -n '${NAMESPACE}' get pod playpen-0 -o jsonpath='{.metadata.uid}' 2>/dev/null) && [ -n \"\${uid}\" ] && [ \"\${uid}\" != '${OLD_UID}' ]"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" wait --for=condition=Ready pod/playpen-0 --timeout=120s
POD_IP=$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod playpen-0 -o jsonpath='{.status.podIP}')
sudo ip route replace "${POD_IP}/32" via "${NODE_IP}"
wait_for "replacement Redfish endpoint" 30 power_is On
[[ $(bmc_fingerprint) == "${FINGERPRINT}" ]] || fail "Redfish certificate changed across pod recreation"
[[ $(pod_exec dd if=/var/lib/playpen/disk.raw bs=1 skip=67108832 count=23 status=none) == playpen-persistent-disk ]] || fail "disk sentinel did not survive pod recreation"
pod_exec test -n "$(pod_exec find /var/lib/playpen/tpm -type f -print -quit)"
pod_exec sh -c "tr '\\0' ' ' </proc/\$(pgrep -f '[q]emu-system-x86_64' | head -n 1)/cmdline" >"${WORK_DIR}/qemu-recreated.args"
grep -q "mac=${CLAIMED_MAC}" "${WORK_DIR}/qemu-recreated.args" || fail "VM MAC changed across pod recreation"

log "all PXE, claim identity, disk, TPM, Redfish, and persistence checks passed"
