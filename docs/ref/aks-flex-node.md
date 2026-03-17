# AKS Flex Node Notes

[AKS Flex Node](https://github.com/Azure/AKSFlexNode) is acting a temporary node join agent for unbounded nodes.
It provides a subcommand `apply` for running a sequences of commands to be applied on the node,
which allows users to perform join/unjoin operations on the node.

These commands are declared as protobuf messages, a few examples:

- [`linux.ConfigureBaseOS`](https://github.com/Azure/AKSFlexNode/blob/06ae2e1c02c0a2facc16e31c8cc3bdff1043b822/components/linux/action.proto#L9)
- [`kubeadm.KubeadmNodeJoin`](https://github.com/Azure/AKSFlexNode/blob/06ae2e1c02c0a2facc16e31c8cc3bdff1043b822/components/kubeadm/action.proto#L9)
- [`kubeadm.KubeadmNodeReset`](https://github.com/Azure/AKSFlexNode/blob/06ae2e1c02c0a2facc16e31c8cc3bdff1043b822/components/kubeadm/action.proto#L52)

We will transit to a new node join implementation in the future with support for
connecting to control plane endpoint for receiving future operations after the initial join.

## Joining a Node to the Cluster

To join a node to the cluster, we require to run steps like downloading and installing
required packages and binaries, then configuring the container runtime and kubelet,
and finally running `kubeadm join` to join the node to the cluster.

The full steps can be found in [`aks-flex-node-install.sh`](cmd/agent/assets/aks-flex-node-install.sh).

## Unjoining a Node from the Cluster

Currently unjoining a node from the cluster requires applying the reset steps manually on the node:

```
/tmp/aks-flex-node-linux-amd64 apply -f - <<EOF
[
  {
    "metadata": {
      "type": "aks.flex.components.kubeadm.KubeadmNodeReset",
      "name": "kubeadm-node-reset"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "aks.flex.components.cri.ResetContainerdService",
      "name": "reset-containerd-service"
    },
    "spec": {}
  }
]
EOF
```

This will 1) reset the kubeadmd and kubelet configuration on the node, and 2) reset the containerd service on the node.
After running this command and deleting the node object inside the cluster, the node will be ready for joining again.