# Unbounded Karpenter plugin

`unbounded-karpenter` is upstream [Karpenter](https://github.com/kubernetes-sigs/karpenter)
compiled with one cloud provider: Unbounded. It autoscales a cluster through
two Unbounded resources and nothing else:

- it reads **MachineClass** and presents its offerings as instance types;
- it creates one **MachineAllocation** per NodeClaim and reports it back.

The Unbounded provider controllers behind those resources launch and release
the machines. The plugin has no cloud SDK, credentials, or per-provider code,
so the same binary works with every provider that serves MachineClass and
MachineAllocation.

## Use

1. Run a provider controller with capacity enabled, and create a MachineClass
   for it. The class must be `Ready`.
2. Install the plugin:

   ```sh
   make karpenter-manifests KARPENTER_IMAGE=<registry>/unbounded-karpenter:<tag>
   kubectl apply --server-side -f deploy/karpenter/rendered/karpenter.yaml
   ```

3. Point a NodePool at the class. No NodeClass wrapper is needed:

   ```yaml
   apiVersion: karpenter.sh/v1
   kind: NodePool
   metadata:
     name: workers
   spec:
     template:
       spec:
         nodeClassRef:
           group: unbounded-cloud.io
           kind: MachineClass
           name: workers
         requirements:
           - key: karpenter.sh/capacity-type
             operator: In
             values: ["on-demand"]
   ```

Provider controllers need no Karpenter-specific flag, role, or start order.

## What an offering needs

Karpenter can use a MachineClass offering that has positive `cpu`, `memory`,
and `pods` capacity and a `kubernetes.io/arch` label. The plugin defaults the
rest: the zone to the class's Site name, the operating system to `linux`, and
a missing price (or one in a currency other than `UNBOUNDED_PRICE_CURRENCY`,
default `USD`) to an estimate from CPU and memory. An offering it cannot use
is skipped and counted in `unbounded_karpenter_unusable_offerings`.
