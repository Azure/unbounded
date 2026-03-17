#!/bin/bash
set -eo pipefail

KUBE_VERSION="${KUBE_VERSION#v}"
arch="$(uname -m)"
case "$arch" in
    "x86_64") arch="amd64" ;;
    "aarch64") arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

curl -L "https://github.com/Azure/AKSFlexNode/releases/download/v0.0.17/aks-flex-node-linux-$arch.tar.gz" | tar -xz
"./aks-flex-node-linux-${arch}" apply -f - <<EOF
[
  {
    "metadata": {
      "type": "aks.flex.components.linux.ConfigureBaseOS",
      "name": "configure-base-os"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "aks.flex.components.linux.DisableDocker",
      "name": "disable-docker"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "aks.flex.components.cri.DownloadCRIBinaries",
      "name": "download-cri-binaries"
    },
    "spec": {
      "containerd_version": "2.0.4",
      "runc_version": "1.2.5"
    }
  },
  {
    "metadata": {
      "type": "aks.flex.components.kubebins.DownloadKubeBinaries",
      "name": "download-kube-binaries"
    },
    "spec": {
      "kubernetes_version": "${KUBE_VERSION}"
    }
  },
  {
    "metadata": {
      "type": "aks.flex.components.cri.StartContainerdService",
      "name": "start-containerd-service"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "aks.flex.components.linux.ConfigureIPTables",
      "name": "configure-iptables"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "aks.flex.components.kubeadm.KubeadmNodeJoin",
      "name": "kubeadm-node-join"
    },
    "spec": {
      "control_plane": {
        "server": "https://${API_SERVER}",
        "certificate_authority_data": "${CA_CERT_BASE64}"
      },
      "kubelet": {
        "bootstrap_auth_info": {
          "token": "${BOOTSTRAP_TOKEN}"
        },
        "node_labels": {
          "kubernetes.azure.com/managed": "false",
          "kubernetes.azure.com/cluster": "${CLUSTER_RG}",
          "machina.project-unbounded.io/machine": "${MACHINA_MACHINE_NAME}"
        }
      }
    }
  }
]
EOF