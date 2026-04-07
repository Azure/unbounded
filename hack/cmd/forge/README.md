# hack/cmd/forge

Forge is a tool for provisioning a control plane and extending it with Datacenter sites. You can have multiple sites
connected to a Forge-built cluster.

Current Forge is an Azure-centric development tool but the plan is to decouple it from AKS to facilitate more robust
testing.

## Build Forge

`make forge`

## Global Flags

These flags apply to all commands:

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--cloud` | `-a` | `AzurePublicCloud` | Azure cloud name |
| `--subscription` | `-s` | `44654aed-2753-4b88-9142-af7132933b6b` | Azure subscription ID |
| `--log-format` | | `text` | Log format (`text` or `json`) |

## Create the Cluster

```bash
NAME="forge-$(cat /dev/urandom | tr -dc 'a-z0-9' | head -c 8)"

bin/forge cluster create --name "$NAME"
```

### `cluster create` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | | Cluster name |
| `--location` | `canadacentral` | Azure location for the cluster |
| `--ssh-dir` | | Directory to place SSH keys |
| `--system-pool-node-sku` | `Standard_D2ads_v6` | VM SKU for system node pool |
| `--system-pool-node-count` | `2` | Number of nodes in the system node pool |
| `--gateway-pool-node-sku` | `Standard_D2ads_v6` | VM SKU for gateway node pool |
| `--gateway-pool-node-count` | `2` | Number of nodes in the gateway node pool |

## Delete the Cluster

```bash
bin/forge cluster delete --name "$NAME"
```

### `cluster delete` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | | Cluster name |

## Add a Site

A site represents one or more pools of machines connected to the cluster. Most of the time people will work with a
single pool in their site. But sometimes a site might have multiple pools.

All `site` subcommands accept these persistent flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--cluster` | | The name of the cluster |
| `--site` | | The name of the site |

```bash
bin/forge site azure add \
  --cluster $NAME \
  --site $NAME-dc1
```

### `site azure add` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--azure` | `AzurePublicCloud` | Azure cloud name |
| `--subscription` | `44654aed-2753-4b88-9142-af7132933b6b` | Azure subscription ID |
| `--location` | `canadacentral` | Azure location |
| `--worker-node-cidr` | `10.1.0.0/16` | CIDR range to use for worker nodes |
| `--add-unbounded-cni-site` | `false` | Add an unbounded-cni site configuration automatically |
| `--ssh-bastion` | `false` | Provision an SSH bastion (jump host) for the site |
| `--ssh-bastion-vm-size` | `Standard_D2ads_v6` | VM size to use for the SSH bastion |
| `--ssh-bastion-disable-direct-access` | `false` | Disable direct SSH access to worker pools, forcing access through the bastion |
| `--ssh-public-key` | | SSH public key (leave empty to generate a new key pair) |
| `--ssh-private-key` | | SSH private key (leave empty to generate a new key pair) |

## Add a Pool

Then add the pool:

```bash
bin/forge site azure add-pool \
  --cluster $NAME \
  --site $NAME-dc1 \
  --name dev1
```

### `site azure add-pool` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--azure` | `AzurePublicCloud` | Azure cloud name |
| `--subscription` | `44654aed-2753-4b88-9142-af7132933b6b` | Azure subscription ID |
| `--location` | `canadacentral` | Azure location |
| `--name` | | Name of the machine pool to add |
| `--count` | `2` | Number of worker nodes to create in the pool |
| `--size` | `standard_d2ads_v6` | VM size to use for worker nodes in the pool |
| `--ssh-user` | | SSH user name for worker nodes in the pool |
| `--ssh-public-key` | | SSH public key (leave empty to generate a new key pair) |
| `--ssh-private-key` | | SSH private key (leave empty to generate a new key pair) |
| `--ssh-backend-port` | `22` | Backend SSH port |
| `--ssh-frontend-port-start` | `22001` | Starting frontend port for SSH |
| `--ssh-frontend-port-end` | `22999` | Ending frontend port for SSH |

## Viewing Inventory

```bash
# use --output machina to output a list of machines that is compatible with Machina

bin/forge site azure inventory \
    --cluster $NAME \
    --site $NAME-dc1 \
    --match-prefix $NAME-dc1-dev1 \
    --output machina
```

### `site azure inventory` flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o` | | Output format (`machina`, `ssh`) |
| `--namespace` | | `default` | Kubernetes namespace for machina output |
| `--match-prefix` | | | Only include machines whose VM name starts with this prefix |
| `--machina-bastion` | | `false` | When used with `--output=machina`, configure each Machine CR with `spec.ssh.bastion` using the bastion's public IP |
| `--machina-ssh-secret-ref` | | | Secret reference for `spec.ssh.privateKeyRef` in format `[$namespace/]$name[:$key]` (default namespace: machina-system) |
| `--machina-bastion-ssh-secret-ref` | | | Secret reference for `spec.ssh.bastion.privateKeyRef` in format `[$namespace/]$name[:$key]` (default namespace: machina-system) |
| `--machina-ssh-username` | | `kubedev` | SSH username for `spec.ssh.username` on each Machine CR |
| `--machina-bastion-ssh-username` | | `kubedev` | SSH username for `spec.ssh.bastion.username` on each Machine CR |
