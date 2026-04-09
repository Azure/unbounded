# nebius-ssh

Provision a CPU VM on Nebius Cloud with VPC networking and SSH access using the `nebius` CLI.

## Prerequisites

- [Nebius CLI](https://docs.nebius.com/cli/install) installed and authenticated
- [jq](https://jqlang.github.io/jq/download/)

Verify your CLI is configured:

```bash
nebius config list
```

## Usage

```
./create.sh network
./create.sh instance <name> <ssh-public-key-path>
./create.sh clean instance <name>
./create.sh clean network
```

### 1. Create the network

Sets up a private IP pool (`172.20.0.0/16`), VPC network, and subnet.

```bash
./create.sh network
```

```
==> Creating private IP pool 'nebius-unbounded-pool' (CIDR 172.20.0.0/16) ...
  Pool ID: vpcpool-e00...

==> Creating VPC network 'nebius-unbounded-network' ...
  Network ID: vpcnetwork-e00...

==> Creating subnet 'nebius-unbounded-subnet' (CIDR 172.20.0.0/16) ...
  Subnet ID: vpcsubnet-e00...

==> Network resources created:
  Pool:    vpcpool-e00...     (nebius-unbounded-pool, 172.20.0.0/16)
  Network: vpcnetwork-e00...  (nebius-unbounded-network)
  Subnet:  vpcsubnet-e00...   (nebius-unbounded-subnet)
```

### 2. Create a VM instance

Creates a boot disk (64 GiB, Ubuntu 22.04) and a CPU VM (`cpu-d3`, `4vcpu-16gb`) with a public IP and your SSH key.

```bash
./create.sh instance my-machine ~/.ssh/id_rsa.pub
```

```
==> Looking up subnet 'nebius-unbounded-subnet' ...
  Subnet ID: vpcsubnet-e00...

==> Creating boot disk 'my-machine-boot-disk' (64 GiB, network_ssd) ...
  Disk ID: computedisk-e00...

==> Creating VM 'my-machine' (platform=cpu-d3, preset=4vcpu-16gb) ...
  Instance ID: computeinstance-e00...

==> Instance resources created:
  Disk:     computedisk-e00...      (my-machine-boot-disk)
  Instance: computeinstance-e00...  (my-machine)

  Public IP: 203.0.113.42
  SSH:       ssh ubuntu@203.0.113.42
```

### 3. Clean up an instance

Deletes the VM and its boot disk by name.

```bash
./create.sh clean instance my-machine
```

```
==> Looking up instance 'my-machine' ...
==> Deleting instance 'my-machine' (computeinstance-e00...) ...
  Deleted instance: computeinstance-e00...

==> Looking up disk 'my-machine-boot-disk' ...
==> Deleting disk 'my-machine-boot-disk' (computedisk-e00...) ...
  Deleted disk: computedisk-e00...

==> Instance cleanup complete.
```

### 4. Clean up the network

Deletes the subnet, network, and pool. Run this after all instances are removed.

```bash
./create.sh clean network
```

```
==> Deleting subnet 'nebius-unbounded-subnet' (vpcsubnet-e00...) ...
  Deleted subnet: vpcsubnet-e00...

==> Deleting network 'nebius-unbounded-network' (vpcnetwork-e00...) ...
  Deleted network: vpcnetwork-e00...

==> Deleting pool 'nebius-unbounded-pool' (vpcpool-e00...) ...
  Deleted pool: vpcpool-e00...

==> Network cleanup complete.
```

## Configuration

Defaults are defined at the top of `create.sh`:

| Variable       | Default                    | Description                  |
|----------------|----------------------------|------------------------------|
| `POOL_NAME`    | `nebius-unbounded-pool`    | Private IP pool name         |
| `NETWORK_NAME` | `nebius-unbounded-network` | VPC network name             |
| `SUBNET_NAME`  | `nebius-unbounded-subnet`  | Subnet name                  |
| `SUBNET_CIDR`  | `172.20.0.0/16`            | Private subnet CIDR          |
| `DISK_SIZE_GB` | `64`                       | Boot disk size in GiB        |
| `DISK_TYPE`    | `network_ssd`              | Boot disk type               |
| `IMAGE_FAMILY` | `ubuntu22.04-driverless`   | OS image family              |
| `PLATFORM`     | `cpu-d3`                   | Compute platform (AMD Genoa) |
| `PRESET`       | `4vcpu-16gb`               | CPU/memory preset            |
| `SSH_USER`     | `ubuntu`                   | User created via cloud-init  |

## End-to-End Flow: Joining a Nebius VM to an AKS Cluster

This walks through the full flow of deploying the machina controller, creating
a Nebius VM, and joining it as a worker node.

### Prerequisites

- An AKS cluster with kubeconfig configured
- The `kubectl unbounded` plugin installed
- The [Nebius CLI](https://docs.nebius.com/cli/install) installed and authenticated

### Step 1: Deploy the machina controller

Install the CRDs and deploy the controller to your cluster:

```bash
kubectl apply -f deploy/machina/crd/
kubectl apply -f deploy/machina/
```

### Step 2: Run setup and generate SSH key

This creates the RBAC resources, kubeadm configs, a bootstrap token, and an
Ed25519 SSH key pair. The public key is saved locally as `unbounded_ed25519.pub`
and the private key is stored as a Secret in the `unbounded-kube` namespace.

```bash
kubectl unbounded setup
```

### Step 3: Create Nebius network and apply the Site resource

Create the Nebius VPC networking (IP pool, network, subnet) and then apply the
Site resource so the cluster knows about the Nebius node CIDR and pod CIDR
assignments.

```bash
./create.sh network
kubectl apply -f site-nebius.yaml
```

The `site-nebius.yaml` declares the Nebius subnet CIDR (`172.20.0.0/16`) and
the pod CIDR block (`10.200.0.0/16`) to assign to nodes at that site:

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: Site
metadata:
  name: site-nebius
spec:
  nodeCidrs:
    - 172.20.0.0/16
  podCidrAssignments:
    - assignmentEnabled: true
      cidrBlocks:
        - 10.200.0.0/16
      nodeBlockSizes:
        ipv4: 24
      priority: 100
```

### Step 4: Create a Nebius VM with the SSH public key

Use the generated public key from step 2 to create a VM on Nebius:

```bash
./create.sh instance test1 ./unbounded_ed25519.pub
```

Note the public IP from the output (e.g. `<NEBIUS_PUBLIC_IP>`).

### Step 5: Create a Machine pointing to the Nebius VM

Create a Machine resource targeting the Nebius VM's public IP. The `--ssh-username`
flag sets the SSH username for this specific machine (the Nebius default is
`ubuntu`):

```bash
kubectl unbounded create test1 --host <NEBIUS_PUBLIC_IP> --ssh-username ubuntu
```

The controller will probe the machine, SSH in, run the bootstrap script, and
wait for the node to join the cluster.

### Result

Once the machine reaches the `Ready` phase, the Nebius VM appears as a
Kubernetes node:

```
$ kubectl get nodes -o wide
NAME                                 STATUS   ROLES    AGE    VERSION   INTERNAL-IP   EXTERNAL-IP     OS-IMAGE             KERNEL-VERSION       CONTAINER-RUNTIME
aks-gateway-32034872-vmss000000      Ready    <none>   94m    v1.34.2   172.16.2.4    23.100.126.32   Ubuntu 22.04.5 LTS   5.15.0-1102-azure    containerd://1.7.30-2
aks-system-28199687-vmss000000       Ready    <none>   103m   v1.34.2   172.16.1.4    <none>          Ubuntu 22.04.5 LTS   5.15.0-1102-azure    containerd://1.7.30-2
aks-system-28199687-vmss000001       Ready    <none>   103m   v1.34.2   172.16.1.6    <none>          Ubuntu 22.04.5 LTS   5.15.0-1102-azure    containerd://1.7.30-2
aks-system-28199687-vmss000002       Ready    <none>   103m   v1.34.2   172.16.1.5    <none>          Ubuntu 22.04.5 LTS   5.15.0-1102-azure    containerd://1.7.30-2
computeinstance-e00yyy3zge8n23pawz   Ready    <none>   8s     v1.34.2   172.20.0.34   <none>          Ubuntu 22.04.5 LTS   5.15.0-170-generic   containerd://2.0.4

$ kubectl get machine
NAME    HOST                 PHASE   AGE
test1   <NEBIUS_PUBLIC_IP>   Ready   18s
```

The Nebius VM (`computeinstance-e00yyy3zge8n23pawz`) is now a fully functional
worker node with an internal IP from the Nebius subnet (`172.20.0.34`).
