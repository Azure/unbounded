# hack/cmd/forge

Forge is a tool for provisioning a control plane and extending it with Datacenter sites. You can have multiple sites
connected to a Forge-built cluster.

Current Forge is an Azure-centric development tool but the plan is to decouple it from AKS to facilitate more robust
testing.

## Build Forge

`make forge`

## Create the Cluster

```bash
NAME="forge-$(cat /dev/urandom | tr -dc 'a-z0-9' | head -c 8)"

bin/forge cluster create \
    --unbounded-cni-release-url <path-to-unbounde-cni-tarball> \
    --name "$NAME"
```

## Add a site

A site represents one or more pools of machines connected to the cluster. Most of the time people will work with a
single pool in their site. But sometimes a site might have multiple pools.

```bash
bin/forge site azure add \
  --cluster $NAME \
  --site $NAME-dc1
```

Then add the pool:

```bash
bin/forge site azure add-pool \
  --cluster $NAME \
  --site $NAME-dc1 \
  --name dev1
```

## Viewing inventory

```bash
# use --output machina to output a list of machines that is compatible with Machina

bin/forge site azure inventory \
    --site $NAME-dc1 \
    --match-prefix $NAME-dc1-dev1 \
    --output machina
```
