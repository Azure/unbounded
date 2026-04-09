# Unbounded Kubernetes - Kubernetes without Borders

[![Release](https://img.shields.io/github/v/release/Azure/unbounded-kube?style=flat-square)](https://github.com/Azure/unbounded-kube/releases/latest)
[![Go CI](https://img.shields.io/github/actions/workflow/status/Azure/unbounded-kube/go-ci.yaml?branch=main&label=CI&style=flat-square)](https://github.com/Azure/unbounded-kube/actions/workflows/go-ci.yaml)
[![License](https://img.shields.io/github/license/Azure/unbounded-kube?style=flat-square)](LICENSE)

## WARNING: Project is in Early Development

**This project is in early development and while we feel comfortable it can be used for experimentation and prototyping right now there are still rough edges and potential for breaking changes. Please report your experiences through the Issue Tracker so we can help!**

The unbounded-kube project enables Kubernetes operators to provision worker nodes anywhere and connect them back to a
central control plane.

## Setup 

1. A running Kubernetes cluster with a `kubeconfig` file that has access to the cluster.
2. `kubectl-unbounded` installed and on your `PATH`. Download it from the releases or build it from source
   with `make kubectl-unbounded`.
3. One or more nodes with the label `unbounded-kube.io/unbounded-net-gateway=true` that can be used as the network
   gateway for your unbounded sites. These nodes also need to allow UDP traffic on ports `51820-51899` for WireGuard.

## Initial Site Creation

A "Site" is a location where you have Machines you want to connect back to your cluster. You can have multiple sites. 
Each site has its own network configuration and set of machines. You use `kubectl-unbounded` to initialize a new site
and then register machines to provision.

| Option                | Description                                                                                                                                                                     |
|-----------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--name`              | Name of the site.                                                                                                                                                               |
| `--cluster-node-cidr` | The node CIDR of your existing cluster.                                                                                                                                         |
| `--cluster-pod-cidr`  | The pod CIDR of your existing cluster.                                                                                                                                          |
| `--node-cidr`         | The node CIDR for your new site. This is the CIDR range that will be used for the nodes in this site. It should not overlap with your existing cluster's node CIDR or pod CIDR. |
| `--pod-cidr`          | The pod CIDR for your new site. This is the CIDR range that will be used for the pods in this site. It should not overlap with your existing cluster's node CIDR or pod CIDR.   |

```bash
kubectl unbounded site init \
  --name hello-unbounded \
  --cluster-node-cidr <cidr> \
  --cluster-pod-cidr <cidr \
  --node-cidr <cidr> \
  --pod-cidr <cidr
```

## Add your first Machine!

| Option                | Description                                                                                                                                                  |
|-----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--site`              | Name of the site.                                                                                                                                            |
| `--name`              | Name of the machine. This will become the node name in Kubernetes once the machine is provisioned and joined to the cluster.                                 |
| `--host`              | The host and port where the machine can be reached. This should be in the format `<ip>[:port]`. If no port is specified, the default port `22` will be used. |
| `--ssh-username`      | The username to use when connecting to the machine over SSH.                                                                                                 | 
| `--ssh-private-key`   | The path to the SSH private key to use when connecting to the machine over SSH. This key should have access to the machine specified in `--host`.            |

```bash
kubectl unbounded site add-machine \
  --site hello-unbounded \
  --name node0 \
  --host <ip>[:port] \
  --ssh-username <username> \
  --ssh-private-key <path-to-ssh-key>
```

## Contributing

This project welcomes contributions and suggestions. See [CONTRIBUTING.md](CONTRIBUTING.md) for
details.

## Third-Party Dependencies

This project uses third-party open source libraries. See the [NOTICE](NOTICE) file for
attributions and license information.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of
Microsoft trademarks or logos is subject to and must follow
[Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or
imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those
third-party's policies.
