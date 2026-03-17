# Unbounded Kubernetes - Kubernetes without Boundaries

Core components of Project Unbounded which enable Kubernetes users to run worker Nodes anywhere and connect them back to your central control plane.

# Getting Started

We will have binaries published soon. In the meantime you need to build from source:

## Install `kubectl-unbounded`

```bash
make kubectl-unbounded
export PATH=$PATH:$(pwd)/bin

# test it works
kubectl unbounded --help
```

## Prerequisites

1. `kubectl-unbounded` installed and on your `PATH`
2. Kubernetes cluster that you have access to a `kubeconfig` file for.
   - Kube-unbounded developers are encouraged to use
         the Forge tool in this repo for development and testing, but any Kubernetes cluster should work. See Forge 
         [README](hack/cmd/forge/README.md) for details.

## CNI Setup

**NOTE: If you're using the Forge tool you can skip this section.**

You need to have one or more nodes with `UDP/51820-51899` open for WireGuard connectivity. That node should become
`<node-name>` in the command below:

```bash
kubectl label node <node-name> "unbounded.aks.azure.com/gateway=true"
```

Download a recent `unbounded-cni` release from https://github.com/azure-management-and-platforms/aks-unbounded-cni/ then
install the CNI.

```
mkdir -p /tmp/unbounded-cni
tar -xzf unbounded-cni.tar.gz -C /tmp/unbounded-cni
kubectl apply -R -f /tmp/unbounded-cni
```

## Install Machina

Machina provisions machines to join them to your cluster.

```bash
kubectl apply -Rf deploy/machina
```

## Adding Nodes

Assume you have a couple running computers somewhere and they are reachable with an `$host:$port` combination. You
also have an SSH key pair for reaching these nodes.

In `unbounded-kube` a place where external nodes are hosted is called a "Site". To bootstrap new nodes you need to know
the `--node-cidr` for the machine's in your Site.

```bash
# If used Forge you can use this command:
FORGE_NAME=<your forge cluster name>

kubectl unbounded setup \
    --ssh-private-key=~/.unbounded-forge/$FORGE_NAME/ssh/$FORGE_NAME \
    --node-cidr=$(az network vnet show -g $FORGE_NAME-dc1 -n main --query "addressSpace.addressPrefixes[0]" -o tsv | tr -d '\n')

# If you DID NOT use Forge you can use this command but you need to fill in the 
# parameters yourself.
kubectl unbounded setup \
    --ssh-private-key=<ssh-private-key-file> \
    --node-cidr=<node-cidr>`

kubectl unbounded create machinemodel mymodel --ssh-username <ssh-user>

kubectl unbounded create worker-01 --host <host-ip-or-name> --port <port>
kubectl unbounded create worker-02 --host <host-ip-or-name> --port <port>

# watch machines. should moved from Ready -> Provisioning -> Provisioned -> Joined
watch 'kubectl get mach'
NAME      HOST            PORT    MODEL     PHASE          NODE                          AGE
worker0   40.85.222.227   51001   mymodel   Joined         pal2forge-dc1-ubdemo-000000   19m
worker1   40.85.222.227   51002   mymodel   Provisioning                                 2s
``` 

