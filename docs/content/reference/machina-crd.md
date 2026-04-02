---
title: "CRD Reference"
weight: 2
description: "API reference for Machine and Image custom resources."
---

API group: `unbounded-kube.io/v1alpha3`

This document describes the two custom resource definitions shipped with the project: **Machine** and **Image**.

## Machine

| Property | Value |
|----------|-------|
| Kind | `Machine` |
| Plural | `machines` |
| Short name | `mach` |
| Scope | Cluster |
| Status subresource | Yes |

**Printer columns:**

| Name | JSON Path | Description |
|------|-----------|-------------|
| Host | `.spec.ssh.host` | SSH target address |
| Phase | `.status.phase` | Current lifecycle phase |
| K8s Version | `.spec.kubernetes.version` | Desired Kubernetes version |
| Age | standard | Time since creation |

### spec.ssh

SSH connection details. When `ssh` is nil, the machina controller skips the Machine entirely.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ssh` | SSHSpec | No | — | SSH connection configuration. |
| `ssh.host` | string | Yes | — | Hostname or IP, optionally with port (e.g. `1.2.3.4:2222`). Port 22 is assumed when omitted. |
| `ssh.username` | string | No | `"azureuser"` | SSH username. |
| `ssh.privateKeyRef` | SecretKeySelector | Yes | — | Reference to a Secret containing the SSH private key. Must reside in the `machina-system` namespace. |
| `ssh.privateKeyRef.name` | string | Yes | — | Secret name. |
| `ssh.privateKeyRef.namespace` | string | Yes | — | Secret namespace (must be `machina-system`). |
| `ssh.privateKeyRef.key` | string | No | `"ssh-privatekey"` | Key within the Secret's `data` map. |
| `ssh.bastion` | BastionSSHSpec | No | — | Optional jump host for the SSH connection. |
| `ssh.bastion.host` | string | Yes | — | Bastion hostname or IP, optionally with port. |
| `ssh.bastion.username` | string | No | `"azureuser"` | Bastion SSH username. |
| `ssh.bastion.privateKeyRef` | *SecretKeySelector | No | Same as `ssh.privateKeyRef` | Bastion SSH key. Falls back to the parent `ssh.privateKeyRef` when omitted. |

### spec.pxe

PXE boot configuration consumed by the metalman controller.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `pxe` | PXESpec | No | — | PXE boot configuration. |
| `pxe.imageRef.name` | string | Yes | — | Name of the Image CR to boot from. |
| `pxe.dhcpLeases` | []DHCPLease | No | — | Static DHCP leases served during PXE boot. |
| `pxe.dhcpLeases[].ipv4` | string | Yes | — | Static IPv4 address to assign. |
| `pxe.dhcpLeases[].mac` | string | Yes | — | NIC MAC address (matched case-insensitively). |
| `pxe.dhcpLeases[].subnetMask` | string | Yes | — | Subnet mask. |
| `pxe.dhcpLeases[].gateway` | string | Yes | — | Default gateway. |
| `pxe.dhcpLeases[].dns` | []string | No | — | DNS server addresses. |
| `pxe.redfish` | RedfishSpec | No | — | BMC access via the Redfish API. |
| `pxe.redfish.url` | string | Yes | — | Redfish endpoint URL. |
| `pxe.redfish.username` | string | Yes | — | Redfish username. |
| `pxe.redfish.deviceID` | string | No | `"1"` | Redfish system device ID. |
| `pxe.redfish.passwordRef` | SecretKeySelector | Yes | — | Secret containing the Redfish password. |

### spec.kubernetes

Kubernetes join configuration.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `kubernetes` | KubernetesSpec | No | — | Kubernetes join settings. |
| `kubernetes.version` | string | No | Cluster version | Desired Kubernetes version (e.g. `"v1.34.0"`). A `v` prefix is added automatically if missing. |
| `kubernetes.nodeRef` | *LocalObjectReference | No | — | Reference to the corresponding Node object. Set by the controller. |
| `kubernetes.nodeLabels` | map[string]string | No | — | Labels to apply to the Node (not yet propagated by the machina controller). |
| `kubernetes.bootstrapTokenRef.name` | string | Yes | — | Name of the bootstrap token Secret in `kube-system`. |

### spec.operations

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `operations.rebootCounter` | int64 | No | `0` | Triggers a reboot when the spec value exceeds the status value. |
| `operations.reimageCounter` | int64 | No | `0` | Triggers a PXE reimage when the spec value exceeds the status value. |

### status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Current lifecycle phase (see table below). |
| `message` | string | Human-readable status message. |
| `ssh.fingerprint` | string | SSH host key fingerprint (not yet implemented). |
| `redfish.certFingerprint` | string | BMC TLS certificate SHA-256 fingerprint. Set by metalman using TOFU. |
| `tpm.ekPublicKey` | string | TPM endorsement key in PEM format. Set by metalman attestation using TOFU. |
| `operations.rebootCounter` | int64 | Last-acted reboot counter value. |
| `operations.reimageCounter` | int64 | Last-acted reimage counter value. |
| `conditions` | []Condition | Standard Kubernetes conditions (see below). |

### Conditions

| Type | Set By | Description |
|------|--------|-------------|
| `SSHReachable` | machina | `True` / `False` based on a TCP probe to the SSH port. |
| `Provisioning` | machina | `True` while the install script is running over SSH. `lastTransitionTime` records when provisioning started, used to detect stale provisioning attempts (e.g. after a controller restart). |
| `Provisioned` | machina | `True` after successful SSH provisioning. `ObservedGeneration` tracks the spec generation. |
| `PoweredOff` | metalman | Tracks BMC power state during a reboot cycle. Removed after power-on completes. Not defined as a CRD type constant; set directly by the metalman redfish reconciler. |
| `BootOrderConfigSupported` | metalman | Set to `False` when the BMC does not support boot order configuration. Not defined as a CRD type constant; set directly by the metalman redfish reconciler. |
| `Reimaged` | metalman | `False`/`Pending` during reimage; `True`/`Succeeded` after `/pxe/disable`. Stale `False` conditions are removed after a 30-minute timeout. |

### Phase lifecycle

The machina controller drives the following phases:

| Phase | Meaning | Requeue interval |
|-------|---------|------------------|
| `Pending` | SSH is unreachable. | 30 s |
| `Provisioning` | Install script is running over SSH. | — |
| `Joining` | Provisioned; waiting for a Node with the matching label. | 30 s |
| `Ready` | Node exists, or no `kubernetes` spec is present. | 5 min |
| `Failed` | Provisioning encountered an error. | 60 s |
| `Rebooting` | Reserved for metalman or provider controllers. | — |

### Labels and annotations

**Labels:**

| Label | Applied to | Description |
|-------|-----------|-------------|
| `unbounded-kube.io/machine` | Node | Maps the Node back to its Machine CR. Set during provisioning. |
| `unbounded-kube.io/site` | Machine | Scopes a metalman instance to a subset of Machines. |
| `unbounded-kube.io/default-bootstrap-token` | Secret | Marks a Secret as the default bootstrap token for auto-discovery. |

**Annotations:**

| Annotation | Description |
|-----------|-------------|
| `unbounded-kube.io/provider` | Associates a Machine with a provider controller (extension point). |

### Examples

**Minimal SSH-only Machine:**

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: worker-01
spec:
  ssh:
    host: "10.0.0.50"
    privateKeyRef:
      name: ssh-key
      namespace: machina-system
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

**SSH with bastion:**

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: worker-02
spec:
  ssh:
    host: "192.168.1.100:2222"
    username: ubuntu
    privateKeyRef:
      name: ssh-key
      namespace: machina-system
      key: id_ed25519
    bastion:
      host: "bastion.example.com"
      username: jump
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

**PXE / bare-metal Machine:**

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: baremetal-01
  labels:
    unbounded-kube.io/site: lab
spec:
  ssh:
    host: "10.0.0.60"
    privateKeyRef:
      name: ssh-key
      namespace: machina-system
  pxe:
    imageRef:
      name: ubuntu-24-04
    dhcpLeases:
    - ipv4: "10.0.0.60"
      mac: "aa:bb:cc:dd:ee:ff"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
      dns:
      - "8.8.8.8"
    redfish:
      url: "https://bmc-01.example.com"
      username: admin
      passwordRef:
        name: bmc-password
        namespace: machina-system
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

---

## Image

| Property | Value |
|----------|-------|
| Kind | `Image` |
| Plural | `images` |
| Short name | `img` |
| Scope | Cluster |
| Status subresource | Yes (empty struct) |

**Printer columns:**

| Name | JSON Path | Description |
|------|-----------|-------------|
| Boot Image | `.spec.dhcpBootImageName` | Filename returned in DHCP responses |
| Age | standard | Time since creation |

### spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dhcpBootImageName` | string | No | — | Boot filename included in DHCP responses (e.g. `"shimx64.efi"`). |
| `files` | []File | No | — | List of boot files served over TFTP/HTTP. |
| `files[].path` | string | Yes | — | Request path for TFTP/HTTP. |
| `files[].http` | *HTTPSource | No | — | File sourced from an HTTP URL. Mutually exclusive with `template` and `static`. |
| `files[].http.url` | string | Yes | — | Download URL. |
| `files[].http.sha256` | string | Yes | — | Expected SHA-256 checksum. |
| `files[].http.convert` | string | No | — | Post-download conversion. Supported: `"UnpackQcow2"` (qcow2 to raw+gzip). |
| `files[].template` | *TemplateSource | No | — | File rendered from a Go template. Mutually exclusive with `http` and `static`. |
| `files[].template.content` | string | Yes | — | Go `text/template` string. |
| `files[].static` | *StaticSource | No | — | Inline file content. Mutually exclusive with `http` and `template`. |
| `files[].static.content` | string | Yes | — | Raw file content. |
| `files[].static.encoding` | string | No | — | Content encoding. Supported: `"base64"`. |

### Template data

Templates receive the following data object:

| Field | Type | Description |
|-------|------|-------------|
| `.Machine` | *Machine | The Machine CR that initiated the request. |
| `.Image` | *Image | The Image CR being served. |
| `.ApiserverURL` | string | External Kubernetes API server URL. |
| `.ServeURL` | string | External metalman HTTP URL. |

### File resolution

- **HTTP** files are downloaded to a content-addressed cache (`<cache>/sha256/<hash>`). A `503` is returned while the download is still in progress.
- **Template** files are rendered per-Machine on every request.
- **Static** files are served as-is, or base64-decoded when `encoding: base64` is set.

### Example

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Image
metadata:
  name: ubuntu-24-04
spec:
  dhcpBootImageName: shimx64.efi
  files:
  - path: shimx64.efi
    http:
      url: https://releases.ubuntu.com/24.04.2/netboot/amd64/bootx64.efi
      sha256: 6fe6e1bcbe6cf6baec8e056d40361ca1aa715cc04ddcc2855351de060b84350b
  - path: grub/grub.cfg
    template:
      content: |
        set timeout=5
        menuentry "Install" {
          linux /vmlinuz autoinstall ds=nocloud-net;s={{ .ServeURL }}/cloud-init/ ---
          initrd /initrd
        }
  - path: cloud-init/user-data
    template:
      content: |
        #cloud-config
        hostname: {{ .Machine.Name }}
  - path: disk.img.gz
    http:
      url: https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
      sha256: 5c3ddb00f60bc455dac0862fabe9d8bacec46c33ac1751143c5c3683404b110d
      convert: UnpackQcow2
```

---

## CRD relationships

```
Machine.spec.pxe.imageRef ──────────────► Image        (by name)
Machine.spec.ssh.privateKeyRef ─────────► Secret       (machina-system namespace)
Machine.spec.pxe.redfish.passwordRef ──► Secret
Machine.spec.kubernetes.bootstrapTokenRef ► Secret     (kube-system namespace)
Machine ◄──── unbounded-kube.io/machine ────► Node     (bidirectional via label)
```

## See Also

- **[SSH Guide]({{< relref "guides/ssh" >}})** -- SSH provisioning walkthrough
  using these CRDs.
- **[PXE Guide]({{< relref "guides/pxe" >}})** -- Bare-metal provisioning
  walkthrough using Machine and Image.
- **[Networking CRDs]({{< relref "reference/networking/custom-resources" >}})**
  -- Site, GatewayPool, and related CRDs from unbounded-net.
- **[CLI Reference]({{< relref "reference/cli" >}})** -- The `kubectl unbounded`
  commands that create these resources.
- **[Architecture]({{< relref "reference/architecture" >}})** -- How these
  CRDs drive the provisioning pipelines.
