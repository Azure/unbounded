#!/usr/bin/env bash
set -euo pipefail

# These host-specific defaults are intentionally explicit so this helper does
# not depend on hidden runner state.
readonly REQUIRED_VALUE="__REQUIRED__"
readonly SSH_USERNAME="ubuntu"
readonly PROXMOX_IMAGE_FILE="${PROXMOX_IMAGE_FILE:-}"
readonly PROXMOX_STORAGE="${PROXMOX_STORAGE:-$REQUIRED_VALUE}"
readonly PROXMOX_CLOUDINIT_STORAGE="${PROXMOX_CLOUDINIT_STORAGE:-${PROXMOX_STORAGE}}"
readonly PROXMOX_BRIDGE="${PROXMOX_BRIDGE:-vmbr0}"
readonly PROXMOX_GATEWAY="${PROXMOX_GATEWAY:-$REQUIRED_VALUE}"
readonly PROXMOX_IP_PREFIX="${PROXMOX_IP_PREFIX:-$REQUIRED_VALUE}"
readonly PROXMOX_IP_CIDR_SUFFIX="${PROXMOX_IP_CIDR_SUFFIX:-24}"
readonly PROXMOX_CPU_CORES="${PROXMOX_CPU_CORES:-4}"
readonly PROXMOX_MEMORY_MB="${PROXMOX_MEMORY_MB:-8192}"
readonly PROXMOX_DISK_GB="${PROXMOX_DISK_GB:-32}"
readonly PROXMOX_SSH_TIMEOUT_SECONDS="${PROXMOX_SSH_TIMEOUT_SECONDS:-300}"
readonly PROXMOX_SSH_RETRY_SECONDS="${PROXMOX_SSH_RETRY_SECONDS:-5}"
readonly PROXMOX_IMAGE_BASE_URL="${PROXMOX_IMAGE_BASE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release}"
readonly PROXMOX_IMAGE_CACHE_DIR="${PROXMOX_IMAGE_CACHE_DIR:-${HOME}/.cache/unbounded/proxmox}"
readonly PROXMOX_IMAGE_ARCH_RAW="$(uname -m)"

case "$PROXMOX_IMAGE_ARCH_RAW" in
  x86_64)
    readonly PROXMOX_IMAGE_ARCH="amd64"
    ;;
  aarch64|arm64)
    readonly PROXMOX_IMAGE_ARCH="arm64"
    ;;
  *)
    die "unsupported architecture for default Proxmox image download: ${PROXMOX_IMAGE_ARCH_RAW}"
    ;;
esac

readonly PROXMOX_DEFAULT_IMAGE_NAME="ubuntu-24.04-minimal-cloudimg-${PROXMOX_IMAGE_ARCH}.img"
readonly PROXMOX_DEFAULT_IMAGE_URL="${PROXMOX_IMAGE_BASE_URL}/${PROXMOX_DEFAULT_IMAGE_NAME}"

usage() {
  cat >&2 <<'EOF'
Usage:
  proxmox-ssh-quickstart-vms.sh create --state-file PATH --ssh-public-key PATH --vm-prefix PREFIX
  proxmox-ssh-quickstart-vms.sh print-env --state-file PATH
  proxmox-ssh-quickstart-vms.sh destroy --state-file PATH

Required environment for create:
  PROXMOX_STORAGE           Proxmox storage for the root disk
  PROXMOX_GATEWAY           IPv4 gateway for the VM network
  PROXMOX_IP_PREFIX         First three octets for deterministic VM IPs (example: 192.168.50)

Optional environment for create:
  PROXMOX_IMAGE_FILE        Local Ubuntu cloud image path to import with qm importdisk
                             (default: cached Ubuntu 24.04 minimal cloud image)
  PROXMOX_CLOUDINIT_STORAGE Proxmox storage for the cloud-init disk (defaults to PROXMOX_STORAGE)
  PROXMOX_BRIDGE            Proxmox bridge for net0 (default: vmbr0)
  PROXMOX_IP_CIDR_SUFFIX    Static IPv4 prefix length (default: 24)
  PROXMOX_CPU_CORES         VM CPU cores (default: 4)
  PROXMOX_MEMORY_MB         VM memory in MiB (default: 8192)
  PROXMOX_DISK_GB           Root disk size after import (default: 32)
  PROXMOX_SSH_TIMEOUT_SECONDS  SSH readiness timeout in seconds (default: 300)
  PROXMOX_SSH_RETRY_SECONDS    Delay between SSH retries in seconds (default: 5)
  PROXMOX_IMAGE_BASE_URL    Base URL for the default Ubuntu cloud image download
  PROXMOX_IMAGE_CACHE_DIR   Cache directory for downloaded cloud images
EOF
  return 1
}

STATE_FILE=""

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || die "required command not found: ${name}"
}

require_file() {
  local path="$1"
  local label="$2"
  [[ -r "$path" ]] || die "${label} does not exist or is not readable: ${path}"
}

require_value() {
  local value="$1"
  local label="$2"
  [[ "$value" != "$REQUIRED_VALUE" ]] || die "set ${label} before running create"
}

note() {
  printf 'info: %s\n' "$*" >&2
}

validate_ipv4_address() {
  local value="$1"
  local octet=""

  [[ "$value" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  IFS='.' read -r -a octets <<<"$value"
  for octet in "${octets[@]}"; do
    (( octet >= 0 && octet <= 255 )) || return 1
  done
}

validate_ipv4_prefix() {
  local value="$1"
  local octet=""

  [[ "$value" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){2}$ ]] || return 1
  IFS='.' read -r -a octets <<<"$value"
  for octet in "${octets[@]}"; do
    (( octet >= 0 && octet <= 255 )) || return 1
  done
}

validate_integer_range() {
  local value="$1"
  local label="$2"
  local minimum="$3"
  local maximum="$4"

  [[ "$value" =~ ^[0-9]+$ ]] || die "${label} must be an integer"
  (( value >= minimum && value <= maximum )) || die "${label} must be between ${minimum} and ${maximum}"
}

validate_positive_integer() {
  local value="$1"
  local label="$2"

  [[ "$value" =~ ^[0-9]+$ ]] || die "${label} must be an integer"
  (( value > 0 )) || die "${label} must be greater than zero"
}

validate_state_line() {
  local key="$1"
  local value="$2"

  case "$key" in
    VM1_NAME|VM2_NAME)
      [[ "$value" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || die "malformed state file ${STATE_FILE}: invalid ${key}"
      ;;
    VM1_IP|VM2_IP)
      validate_ipv4_address "$value" || die "malformed state file ${STATE_FILE}: invalid ${key}"
      ;;
    VM1_VMID|VM2_VMID)
      [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "malformed state file ${STATE_FILE}: invalid ${key}"
      ;;
    SSH_USERNAME)
      [[ "$value" =~ ^[a-z_][a-z0-9_-]*$ ]] || die "malformed state file ${STATE_FILE}: invalid ${key}"
      ;;
    *)
      die "malformed state file ${STATE_FILE}: unexpected key ${key}"
      ;;
  esac
}

require_nonempty_state_value() {
  local key="$1"
  local value="$2"

  [[ -n "$value" ]] || die "malformed state file ${STATE_FILE}: ${key} must not be empty"
}

load_destroy_state() {
  local line=""
  local key=""
  local value=""
  local -A seen_keys=()
  local saw_vm1_name=0
  local saw_vm1_vmid=0
  local saw_vm2_name=0
  local saw_vm2_vmid=0
  local loaded_vm1_name=""
  local vm1_vmid_value=""
  local loaded_vm2_name=""
  local vm2_vmid_value=""

  require_file "$STATE_FILE" "--state-file"

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    [[ "$line" == *=* ]] || die "malformed state file ${STATE_FILE}: expected KEY=VALUE lines"
    key="${line%%=*}"
    value="${line#*=}"
    validate_state_line "$key" "$value"
    if [[ -n "${seen_keys[$key]+x}" ]]; then
      die "malformed state file ${STATE_FILE}: duplicate key ${key}"
    fi
    seen_keys[$key]=1
    case "$key" in
      VM1_NAME)
        require_nonempty_state_value "$key" "$value"
        saw_vm1_name=1
        loaded_vm1_name="$value"
        ;;
      VM1_VMID)
        require_nonempty_state_value "$key" "$value"
        saw_vm1_vmid=1
        vm1_vmid_value="$value"
        ;;
      VM2_NAME)
        require_nonempty_state_value "$key" "$value"
        saw_vm2_name=1
        loaded_vm2_name="$value"
        ;;
      VM2_VMID)
        require_nonempty_state_value "$key" "$value"
        saw_vm2_vmid=1
        vm2_vmid_value="$value"
        ;;
    esac
  done <"$STATE_FILE"

  (( saw_vm1_name == 1 )) || die "malformed state file ${STATE_FILE}: missing VM1_NAME"
  (( saw_vm1_vmid == 1 )) || die "malformed state file ${STATE_FILE}: missing VM1_VMID"
  if (( saw_vm2_name != saw_vm2_vmid )); then
    die "malformed state file ${STATE_FILE}: VM2_NAME and VM2_VMID must either both be present or both be absent"
  fi
  if (( saw_vm2_vmid == 1 )); then
    [[ "$vm1_vmid_value" != "$vm2_vmid_value" ]] || die "malformed state file ${STATE_FILE}: VM1_VMID and VM2_VMID must not be the same"
  fi

  VM1_NAME="$loaded_vm1_name"
  VM1_VMID="$vm1_vmid_value"
  VM2_NAME="$loaded_vm2_name"
  VM2_VMID="$vm2_vmid_value"
}

write_destroy_state_file() {
  local vm1_name="$1"
  local vm1_vmid="$2"
  local vm2_name="$3"
  local vm2_vmid="$4"
  local state_dir
  local state_base
  local state_tmp

  state_dir="$(dirname "$STATE_FILE")"
  state_base="$(basename "$STATE_FILE")"
  mkdir -p "$state_dir"
  state_tmp="$(mktemp "${state_dir}/.${state_base}.tmp.XXXXXX")"
  {
    printf 'VM1_NAME=%s\n' "$vm1_name"
    printf 'VM1_VMID=%s\n' "$vm1_vmid"
    if [[ -n "$vm2_name" && -n "$vm2_vmid" ]]; then
      printf 'VM2_NAME=%s\n' "$vm2_name"
      printf 'VM2_VMID=%s\n' "$vm2_vmid"
    fi
  } >"$state_tmp"
  mv "$state_tmp" "$STATE_FILE"
}

print_validated_state() {
  local line=""
  local key=""
  local value=""
  local output=()
  local missing_keys=()
  local -A seen_keys=()
  local saw_vm1_name=0
  local saw_vm1_vmid=0
  local saw_vm1_ip=0
  local saw_vm2_name=0
  local saw_vm2_vmid=0
  local saw_vm2_ip=0
  local saw_ssh_username=0
  local vm1_vmid_value=""
  local vm2_vmid_value=""

  require_file "$STATE_FILE" "--state-file"

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    [[ "$line" == *=* ]] || die "malformed state file ${STATE_FILE}: expected KEY=VALUE lines"
    key="${line%%=*}"
    value="${line#*=}"
    validate_state_line "$key" "$value"
    if [[ -n "${seen_keys[$key]+x}" ]]; then
      die "malformed state file ${STATE_FILE}: duplicate key ${key}"
    fi
    seen_keys[$key]=1
    case "$key" in
      VM1_NAME)
        require_nonempty_state_value "$key" "$value"
        saw_vm1_name=1
        ;;
      VM1_VMID)
        require_nonempty_state_value "$key" "$value"
        saw_vm1_vmid=1
        vm1_vmid_value="$value"
        ;;
      VM1_IP)
        require_nonempty_state_value "$key" "$value"
        saw_vm1_ip=1
        ;;
      VM2_NAME)
        require_nonempty_state_value "$key" "$value"
        saw_vm2_name=1
        ;;
      VM2_VMID)
        require_nonempty_state_value "$key" "$value"
        saw_vm2_vmid=1
        vm2_vmid_value="$value"
        ;;
      VM2_IP)
        require_nonempty_state_value "$key" "$value"
        saw_vm2_ip=1
        ;;
      SSH_USERNAME)
        require_nonempty_state_value "$key" "$value"
        saw_ssh_username=1
        ;;
    esac
    output+=("${key}=${value}")
  done <"$STATE_FILE"

  (( saw_vm1_name == 1 )) || missing_keys+=("VM1_NAME")
  (( saw_vm1_vmid == 1 )) || missing_keys+=("VM1_VMID")
  (( saw_vm1_ip == 1 )) || missing_keys+=("VM1_IP")
  (( saw_vm2_name == 1 )) || missing_keys+=("VM2_NAME")
  (( saw_vm2_vmid == 1 )) || missing_keys+=("VM2_VMID")
  (( saw_vm2_ip == 1 )) || missing_keys+=("VM2_IP")
  (( saw_ssh_username == 1 )) || missing_keys+=("SSH_USERNAME")

  (( ${#missing_keys[@]} == 0 )) || die "malformed state file ${STATE_FILE}: missing required keys: ${missing_keys[*]}"
  [[ "$vm1_vmid_value" != "$vm2_vmid_value" ]] || die "malformed state file ${STATE_FILE}: VM1_VMID and VM2_VMID must not be the same"

  printf '%s\n' "${output[@]}"
}

destroy_vm() {
  local vm_label="$1"
  local vmid="$2"
  local expected_name="$3"
  local vm_config=""
  local qm_list_output=""

  if ! vm_config="$(qm config "$vmid" 2>/dev/null)"; then
    if ! qm_list_output="$(qm list 2>/dev/null)"; then
      printf '%s\n' "qm list failed while checking ${vm_label} (${vmid}) from state file ${STATE_FILE}" >&2
      return 1
    fi
    if ! grep -qE "^${vmid}[[:space:]]" <<<"$qm_list_output"; then
      note "skipping ${vm_label}: VM ${vmid} does not exist on this Proxmox host"
      return 0
    fi
    printf '%s\n' "qm config failed for ${vm_label} (${vmid}) from state file ${STATE_FILE}" >&2
    return 1
  fi

  grep -qxF "name: ${expected_name}" <<<"$vm_config" || {
    printf '%s\n' "recorded ${vm_label} (${vmid}) does not match expected VM name ${expected_name} in state file ${STATE_FILE}" >&2
    return 1
  }

  if ! qm stop "$vmid" >/dev/null 2>&1; then
    note "qm stop failed for ${vm_label} (${vmid}); continuing to destroy"
  fi

  if ! qm destroy "$vmid" --destroy-unreferenced-disks 1 >/dev/null 2>&1; then
    printf '%s\n' "qm destroy failed for ${vm_label} (${vmid}) from state file ${STATE_FILE}" >&2
    return 1
  fi

  return 0
}

sanitize_vm_prefix() {
  local value="${1,,}"

  value="${value//[^a-z0-9-]/-}"
  value="${value#-}"
  value="${value%-}"
  value="${value:0:54}"
  value="${value%-}"
  [[ -n "$value" ]] || die "--vm-prefix must include at least one letter or digit"
  printf '%s\n' "$value"
}

derive_ip_octet() {
  local prefix="$1"
  local offset="$2"
  local checksum

  checksum="$(printf '%s' "$prefix" | cksum)"
  checksum="${checksum%% *}"
  printf '%s\n' "$((checksum % 180 + 20 + offset))"
}

ipv4_to_int() {
  local value="$1"
  local o1=""
  local o2=""
  local o3=""
  local o4=""

  IFS='.' read -r o1 o2 o3 o4 <<<"$value"
  printf '%s\n' "$((((o1 << 24) | (o2 << 16) | (o3 << 8) | o4)))"
}

cidr_mask() {
  local suffix="$1"

  if (( suffix == 0 )); then
    printf '0\n'
    return 0
  fi

  printf '%s\n' "$((((0xFFFFFFFF << (32 - suffix)) & 0xFFFFFFFF)))"
}

validate_gateway_matches_vm_subnet() {
  local gateway_ip="$1"
  local candidate_vm_ip="$2"
  local mask=""
  local gateway_int=""
  local vm_int=""

  mask="$(cidr_mask "$PROXMOX_IP_CIDR_SUFFIX")"
  gateway_int="$(ipv4_to_int "$gateway_ip")"
  vm_int="$(ipv4_to_int "$candidate_vm_ip")"

  (( (gateway_int & mask) == (vm_int & mask) )) || die "PROXMOX_GATEWAY must be within the derived VM subnet (${candidate_vm_ip}/${PROXMOX_IP_CIDR_SUFFIX})"
  [[ "$gateway_ip" != "$candidate_vm_ip" ]] || die "PROXMOX_GATEWAY must not match the derived VM IP (${candidate_vm_ip})"
}

validate_proxmox_storage() {
  local storage_name="$1"
  local label="$2"

  pvesm status --storage "$storage_name" >/dev/null 2>&1 || die "${label} does not exist on this Proxmox host: ${storage_name}"
}

validate_proxmox_bridge() {
  ip link show "$PROXMOX_BRIDGE" >/dev/null 2>&1 || die "PROXMOX_BRIDGE does not exist on this Proxmox host: ${PROXMOX_BRIDGE}"
}

private_key_path_from_public_key() {
  local public_key_path="$1"

  [[ "$public_key_path" == *.pub ]] || die "--ssh-public-key must end with .pub so the matching private key can be derived for SSH readiness checks"
  printf '%s\n' "${public_key_path%.pub}"
}

next_vmid() {
  local value=""

  if ! value="$(pvesh get /cluster/nextid)"; then
    printf 'failed to allocate next Proxmox VMID\n' >&2
    return 1
  fi
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    printf 'pvesh returned an invalid Proxmox VMID: %s\n' "$value" >&2
    return 1
  fi
  printf '%s\n' "$value"
}

resolve_proxmox_image_file() {
  local image_file="$PROXMOX_IMAGE_FILE"

  if [[ -n "$image_file" ]]; then
    require_file "$image_file" "PROXMOX_IMAGE_FILE"
    printf '%s\n' "$image_file"
    return 0
  fi

  image_file="${PROXMOX_IMAGE_CACHE_DIR}/${PROXMOX_DEFAULT_IMAGE_NAME}"
  mkdir -p "$PROXMOX_IMAGE_CACHE_DIR"

  if [[ ! -f "$image_file" ]]; then
    note "downloading default Ubuntu cloud image to ${image_file}"
    curl -L -o "$image_file" "$PROXMOX_DEFAULT_IMAGE_URL"
  else
    note "using cached Ubuntu cloud image ${image_file}"
  fi

  require_file "$image_file" "resolved Proxmox image file"
  printf '%s\n' "$image_file"
}

ensure_vmid_available() {
  local vmid="$1"
  local label="$2"
  local qm_list_output=""

  if qm config "$vmid" >/dev/null 2>&1; then
    die "${label} ${vmid} already exists on this Proxmox host"
  fi
  if ! qm_list_output="$(qm list 2>/dev/null)"; then
    die "${label} ${vmid} could not be inspected on this Proxmox host"
  fi
  if grep -qE "^${vmid}[[:space:]]" <<<"$qm_list_output"; then
    die "${label} ${vmid} could not be inspected on this Proxmox host"
  fi
}

cleanup_failed_create() {
  local vm1_name="$1"
  local vm1_vmid="$2"
  local vm1_ip="$3"
  local vm2_name="$4"
  local vm2_vmid="$5"
  local vm2_ip="$6"

  if [[ -n "$vm1_vmid" ]]; then
    if ! destroy_vm "VM1_VMID" "$vm1_vmid" "$vm1_name"; then
      if [[ -n "$vm2_vmid" && "$vm2_vmid" != "$vm1_vmid" ]]; then
        write_destroy_state_file "$vm1_name" "$vm1_vmid" "$vm2_name" "$vm2_vmid"
      else
        write_destroy_state_file "$vm1_name" "$vm1_vmid" "" ""
      fi
      die "failed to clean up partially created Proxmox VM ${vm1_name} (${vm1_vmid}); state left in ${STATE_FILE}"
    fi
    rm -f "$STATE_FILE"
  fi
}

existing_state_mentions_ip() {
  local candidate_ip="$1"

  [[ -f "$STATE_FILE" ]] || return 1
  grep -qF "$candidate_ip" "$STATE_FILE"
}

proxmox_config_mentions_ip() {
  local candidate_ip="$1"
  local vmid=""
  local _rest=""
  local qm_list_output=""

  if ! qm_list_output="$(qm list)"; then
    die "failed to list existing Proxmox VMs while checking candidate IP collisions"
  fi

  while read -r vmid _rest; do
    [[ -n "$vmid" ]] || continue
    [[ "$vmid" == "VMID" ]] && continue
    if ! vm_config="$(qm config "$vmid" 2>/dev/null)"; then
      die "failed to inspect existing Proxmox VM ${vmid} while checking candidate IP collisions"
    fi
    if grep -qE "^ipconfig0:.*ip=${candidate_ip}(/|,)" <<<"$vm_config"; then
      return 0
    fi
  done <<<"$qm_list_output"

  return 1
}

guard_candidate_ip() {
  local candidate_ip="$1"
  local label="$2"

  if existing_state_mentions_ip "$candidate_ip"; then
    die "${label} ${candidate_ip} already appears in the current state file ${STATE_FILE}"
  fi

  if proxmox_config_mentions_ip "$candidate_ip"; then
    die "${label} ${candidate_ip} already appears in an existing Proxmox VM ipconfig0"
  fi
}

validate_create_environment() {
  require_command pvesh
  require_command pvesm
  require_command qm
  require_command ip
  require_command ssh
  require_command curl
  require_command cksum
  require_command grep
  require_file "$1" "--ssh-public-key"
  require_value "$PROXMOX_STORAGE" "PROXMOX_STORAGE"
  require_value "$PROXMOX_GATEWAY" "PROXMOX_GATEWAY"
  require_value "$PROXMOX_IP_PREFIX" "PROXMOX_IP_PREFIX"
  validate_ipv4_address "$PROXMOX_GATEWAY" || die "PROXMOX_GATEWAY must be a valid IPv4 address"
  validate_ipv4_prefix "$PROXMOX_IP_PREFIX" || die "PROXMOX_IP_PREFIX must be three IPv4 octets such as 192.168.50"
  validate_integer_range "$PROXMOX_IP_CIDR_SUFFIX" "PROXMOX_IP_CIDR_SUFFIX" 1 32
  validate_positive_integer "$PROXMOX_CPU_CORES" "PROXMOX_CPU_CORES"
  validate_positive_integer "$PROXMOX_MEMORY_MB" "PROXMOX_MEMORY_MB"
  validate_positive_integer "$PROXMOX_DISK_GB" "PROXMOX_DISK_GB"
  validate_positive_integer "$PROXMOX_SSH_TIMEOUT_SECONDS" "PROXMOX_SSH_TIMEOUT_SECONDS"
  validate_positive_integer "$PROXMOX_SSH_RETRY_SECONDS" "PROXMOX_SSH_RETRY_SECONDS"
  (( PROXMOX_SSH_TIMEOUT_SECONDS >= PROXMOX_SSH_RETRY_SECONDS )) || die "PROXMOX_SSH_TIMEOUT_SECONDS must be greater than or equal to PROXMOX_SSH_RETRY_SECONDS"
  validate_proxmox_storage "$PROXMOX_STORAGE" "PROXMOX_STORAGE"
  validate_proxmox_storage "$PROXMOX_CLOUDINIT_STORAGE" "PROXMOX_CLOUDINIT_STORAGE"
  validate_proxmox_bridge

  note "create expects an Ubuntu cloud image with cloud-init support and SSH user ${SSH_USERNAME}"
}

preflight_state_file_path() {
  local state_dir
  local state_base
  local state_tmp

  state_dir="$(dirname "$STATE_FILE")"
  state_base="$(basename "$STATE_FILE")"
  mkdir -p "$state_dir"
  state_tmp="$(mktemp "${state_dir}/.${state_base}.preflight.XXXXXX")" || die "cannot create state file in ${state_dir}"
  rm -f "$state_tmp"
}

create_vm() {
  local vmid="$1"
  local vm_name="$2"

  qm create "$vmid" \
    --name "$vm_name" \
    --ostype l26 \
    --cores "$PROXMOX_CPU_CORES" \
    --memory "$PROXMOX_MEMORY_MB" \
    --net0 "virtio,bridge=${PROXMOX_BRIDGE}"
}

configure_vm() {
  local vmid="$1"
  local vm_name="$2"
  local vm_ip_cidr="$3"
  local ssh_public_key="$4"
  local image_file="$5"

  qm importdisk "$vmid" "$image_file" "$PROXMOX_STORAGE" --format qcow2
  qm set "$vmid" --scsihw virtio-scsi-single --scsi0 "${PROXMOX_STORAGE}:vm-${vmid}-disk-0"
  qm set "$vmid" --ide2 "${PROXMOX_CLOUDINIT_STORAGE}:cloudinit"
  qm set "$vmid" --boot order=scsi0
  qm set "$vmid" --serial0 socket --vga serial0
  qm set "$vmid" --agent enabled=1
  qm set "$vmid" --ciuser "$SSH_USERNAME" --sshkeys "$ssh_public_key"
  qm set "$vmid" --ipconfig0 "ip=${vm_ip_cidr},gw=${PROXMOX_GATEWAY}"
  qm resize "$vmid" scsi0 "${PROXMOX_DISK_GB}G"
  qm start "$vmid"
}

wait_for_ssh() {
  local vm_name="$1"
  local vm_ip="$2"
  local ssh_private_key="$3"
  local ssh_deadline

  ssh_deadline="$((SECONDS + PROXMOX_SSH_TIMEOUT_SECONDS))"
  while (( SECONDS < ssh_deadline )); do
    if ssh -o BatchMode=yes \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=5 \
      -i "$ssh_private_key" \
      "${SSH_USERNAME}@${vm_ip}" true >/dev/null 2>&1; then
      return 0
    fi
    sleep "$PROXMOX_SSH_RETRY_SECONDS"
  done

  die "timed out waiting for SSH on ${vm_name} (${vm_ip})"
}

wait_for_guest_readiness() {
  local vm_name="$1"
  local vm_ip="$2"
  local ssh_private_key="$3"
  local ssh_deadline

  ssh_deadline="$((SECONDS + PROXMOX_SSH_TIMEOUT_SECONDS))"
  while (( SECONDS < ssh_deadline )); do
    if ssh -o BatchMode=yes \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=5 \
      -i "$ssh_private_key" \
      "${SSH_USERNAME}@${vm_ip}" \
      'cloud-init status --wait >/dev/null 2>&1 || ! pgrep -x apt-get >/dev/null 2>&1' >/dev/null 2>&1; then
      return 0
    fi
    sleep "$PROXMOX_SSH_RETRY_SECONDS"
  done

  die "timed out waiting for guest readiness on ${vm_name} (${vm_ip})"
}

write_state_file() {
  local vm1_name="$1"
  local vm1_vmid="$2"
  local vm1_ip="$3"
  local vm2_name="$4"
  local vm2_vmid="$5"
  local vm2_ip="$6"
  local state_dir
  local state_base
  local state_tmp

  state_dir="$(dirname "$STATE_FILE")"
  state_base="$(basename "$STATE_FILE")"
  mkdir -p "$state_dir"
  state_tmp="$(mktemp "${state_dir}/.${state_base}.tmp.XXXXXX")"
  {
    printf 'VM1_NAME=%s\n' "$vm1_name"
    printf 'VM1_VMID=%s\n' "$vm1_vmid"
    printf 'VM1_IP=%s\n' "$vm1_ip"
    printf 'VM2_NAME=%s\n' "$vm2_name"
    printf 'VM2_VMID=%s\n' "$vm2_vmid"
    printf 'VM2_IP=%s\n' "$vm2_ip"
    printf 'SSH_USERNAME=%s\n' "$SSH_USERNAME"
  } >"$state_tmp"
  mv "$state_tmp" "$STATE_FILE"
}

cmd_create() {
  local ssh_public_key=""
  local ssh_private_key=""
  local raw_vm_prefix=""
  local vm_prefix=""
  local vm1_name=""
  local vm2_name=""
  local vm1_vmid=""
  local vm2_vmid=""
  local vm1_octet=""
  local vm2_octet=""
  local vm1_ip=""
  local vm2_ip=""
  local vm1_ip_cidr=""
  local vm2_ip_cidr=""
  local proxmox_image_file=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --state-file)
        [[ $# -ge 2 ]] || die "missing value for --state-file"
        STATE_FILE="$2"
        shift 2
        ;;
      --ssh-public-key)
        [[ $# -ge 2 ]] || die "missing value for --ssh-public-key"
        ssh_public_key="$2"
        shift 2
        ;;
      --vm-prefix)
        [[ $# -ge 2 ]] || die "missing value for --vm-prefix"
        raw_vm_prefix="$2"
        shift 2
        ;;
      *)
        die "unknown create argument: $1"
        ;;
    esac
  done

  [[ -n "$STATE_FILE" ]] || die "--state-file is required"
  [[ -n "$ssh_public_key" ]] || die "--ssh-public-key is required"
  [[ -n "$raw_vm_prefix" ]] || die "--vm-prefix is required"
  [[ ! -e "$STATE_FILE" ]] || die "--state-file already exists: ${STATE_FILE}"
  preflight_state_file_path

  validate_create_environment "$ssh_public_key"
  proxmox_image_file="$(resolve_proxmox_image_file)"

  ssh_private_key="$(private_key_path_from_public_key "$ssh_public_key")"
  require_file "$ssh_private_key" "matching private key"

  vm_prefix="$(sanitize_vm_prefix "$raw_vm_prefix")"
  vm1_name="${vm_prefix}-worker-1"
  vm2_name="${vm_prefix}-worker-2"

  vm1_octet="$(derive_ip_octet "$vm_prefix" 0)"
  vm2_octet="$(derive_ip_octet "$vm_prefix" 1)"
  vm1_ip="${PROXMOX_IP_PREFIX}.${vm1_octet}"
  vm2_ip="${PROXMOX_IP_PREFIX}.${vm2_octet}"
  vm1_ip_cidr="${vm1_ip}/${PROXMOX_IP_CIDR_SUFFIX}"
  vm2_ip_cidr="${vm2_ip}/${PROXMOX_IP_CIDR_SUFFIX}"

  [[ "$vm1_ip" != "$vm2_ip" ]] || die "derived duplicate candidate IPs for ${vm_prefix}: ${vm1_ip}"
  validate_gateway_matches_vm_subnet "$PROXMOX_GATEWAY" "$vm1_ip"
  validate_gateway_matches_vm_subnet "$PROXMOX_GATEWAY" "$vm2_ip"
  guard_candidate_ip "$vm1_ip" "VM1_IP"
  guard_candidate_ip "$vm2_ip" "VM2_IP"

  if ! vm1_vmid="$(next_vmid)"; then
    die "failed to allocate the first Proxmox VMID"
  fi
  ensure_vmid_available "$vm1_vmid" "VM1_VMID"
  if ! create_vm "$vm1_vmid" "$vm1_name"; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "" "$vm2_ip"
    die "failed to create Proxmox VM ${vm1_name} (${vm1_vmid})"
  fi
  if ! vm2_vmid="$(next_vmid)"; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "" "$vm2_ip"
    die "failed to allocate a second Proxmox VMID after creating ${vm1_name} (${vm1_vmid})"
  fi
  if [[ "$vm1_vmid" == "$vm2_vmid" ]]; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
    die "pvesh returned duplicate VMIDs for ${vm_prefix}: ${vm1_vmid}"
  fi
  if qm config "$vm2_vmid" >/dev/null 2>&1; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
    die "VM2_VMID ${vm2_vmid} already exists on this Proxmox host"
  fi
  if ! qm_list_output="$(qm list 2>/dev/null)"; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
    die "VM2_VMID ${vm2_vmid} could not be inspected on this Proxmox host"
  fi
  if grep -qE "^${vm2_vmid}[[:space:]]" <<<"$qm_list_output"; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
    die "VM2_VMID ${vm2_vmid} could not be inspected on this Proxmox host"
  fi
  if ! write_state_file "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"; then
    cleanup_failed_create "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
    die "failed to persist Proxmox VM state after allocating ${vm1_vmid} and ${vm2_vmid}"
  fi

  configure_vm "$vm1_vmid" "$vm1_name" "$vm1_ip_cidr" "$ssh_public_key" "$proxmox_image_file" || die "failed to configure Proxmox VM ${vm1_name} (${vm1_vmid})"
  create_vm "$vm2_vmid" "$vm2_name" || die "failed to create Proxmox VM ${vm2_name} (${vm2_vmid})"
  configure_vm "$vm2_vmid" "$vm2_name" "$vm2_ip_cidr" "$ssh_public_key" "$proxmox_image_file" || die "failed to configure Proxmox VM ${vm2_name} (${vm2_vmid})"

  wait_for_ssh "$vm1_name" "$vm1_ip" "$ssh_private_key" || die "failed waiting for SSH on ${vm1_name} (${vm1_ip})"
  wait_for_ssh "$vm2_name" "$vm2_ip" "$ssh_private_key" || die "failed waiting for SSH on ${vm2_name} (${vm2_ip})"
  wait_for_guest_readiness "$vm1_name" "$vm1_ip" "$ssh_private_key" || die "failed waiting for guest readiness on ${vm1_name} (${vm1_ip})"
  wait_for_guest_readiness "$vm2_name" "$vm2_ip" "$ssh_private_key" || die "failed waiting for guest readiness on ${vm2_name} (${vm2_ip})"
  write_state_file "$vm1_name" "$vm1_vmid" "$vm1_ip" "$vm2_name" "$vm2_vmid" "$vm2_ip"
}

cmd_destroy() {
  local failures=()

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --state-file)
        [[ $# -ge 2 ]] || die "missing value for --state-file"
        STATE_FILE="$2"
        shift 2
        ;;
      *)
        die "unknown destroy argument: $1"
        ;;
    esac
  done

  [[ -n "$STATE_FILE" ]] || die "--state-file is required"
  require_command qm
  load_destroy_state

  if ! destroy_vm "VM1_VMID" "$VM1_VMID" "$VM1_NAME"; then
    failures+=("VM1_VMID=${VM1_VMID}")
  fi

  if [[ -n "${VM2_VMID:-}" && -n "${VM2_NAME:-}" ]] && ! destroy_vm "VM2_VMID" "$VM2_VMID" "$VM2_NAME"; then
    failures+=("VM2_VMID=${VM2_VMID}")
  fi

  if (( ${#failures[@]} > 0 )); then
    die "destroy encountered teardown failures for: ${failures[*]}"
  fi

  rm -f "$STATE_FILE"
}

cmd_print_env() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --state-file)
        [[ $# -ge 2 ]] || die "missing value for --state-file"
        STATE_FILE="$2"
        shift 2
        ;;
      *)
        die "unknown print-env argument: $1"
        ;;
    esac
  done

  [[ -n "$STATE_FILE" ]] || die "--state-file is required"
  print_validated_state
}

main() {
  local cmd="${1:-}"
  [[ -n "$cmd" ]] || return 1
  shift || true
  case "$cmd" in
    create) cmd_create "$@" ;;
    print-env) cmd_print_env "$@" ;;
    destroy) cmd_destroy "$@" ;;
    -h|--help|help) usage ;;
    *) usage ;;
  esac
}

main "$@" || usage
