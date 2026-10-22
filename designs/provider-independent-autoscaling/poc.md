# POC: one Karpenter plugin, Azure and AWS workers

**Result:** the same unchanged Karpenter plugin provisioned, used, and released
Azure and AWS workers in one AKS cluster. The dual-cloud test passed on
2026-10-22 at 14:41:02 UTC.

## What we built

Karpenter talks to two generic Unbounded resources, not directly to either cloud:

- **MachineClass** describes available capacity and references provider configuration.
- **MachineAllocation** records one NodeClaim's demand until its resources are released.

Shared lifecycle controllers turn allocations into `HostAllocate` and
`HostRelease` MachineOperations. The AzureVM and AWSEC2 adapters execute those
operations through their native cloud APIs.

```text
                         One Karpenter plugin
                                  |
                   MachineClass + MachineAllocation
                                  |
                  Shared lifecycle / MachineOperation
                         /                   \
                 AzureVM adapter        AWSEC2 adapter
                         |                   |
                    Azure worker         AWS worker
                         \                   /
                      Same Kubernetes cluster
```

AWS was added without changing the Karpenter source or image. Provider
configuration, credentials, and API calls stayed outside the plugin. Both
workers used the same accepted Unbounded agent artifact.

## What the live test proved

1. Pending workloads scaled from zero to one Ready worker in each cloud, with
   both workloads running at the same time.
2. Restarting all provider-controller and Karpenter pods during provisioning
   preserved the original allocation, operation, and native instance identities.
3. Pods communicated in both directions using Pod IPs and Service DNS.
   `kubectl exec` and logs worked on both workers.
4. PDBs blocked drain for at least 30 seconds on each provider. The allocations
   were fenced against rejoin, but their instances were not released early.
5. Normal termination removed the instances and their actual NICs/disks or
   ENIs/EBS volumes. The test ended with zero workers and zero test demand,
   without direct cloud deletion or finalizer overrides.

The tested offerings were Azure `Standard_D4s_v6` in `eastus2-1` and AWS
`m6i.xlarge` in `us-east-1a`.

## What this means

The provider-independent boundary works for this two-cloud Karpenter slice.
The important lessons were to separate planned identity from native instance
identity, record actual child resources, preserve launch inputs across retries,
and distinguish fencing from permission to release.

This does **not** yet prove CAS/MachinePool support, existing-inventory
reservation, production scale, or the complete failure and recovery matrix.
The [revised design](design.md) covers those remaining requirements.

## Repeat it or inspect the evidence

Use the [runbook's dual-cloud test and cleanup commands](../../hack/karpenter-poc/README.md#finite-dual-cloud-acceptance-and-safe-cleanup).
The [accepted run record](../../hack/karpenter-poc/README.md#accepted-dual-cloud-run)
contains exact identities, image pins, and milestones; its local JSONL is
`tmp/karpenter-poc/aws/dual-acceptance-20261022T143255723461Z.jsonl`.

The environment was retained for testing, with workloads scaled to zero.
AKS system capacity, NAT gateways, storage, and other retained infrastructure
still incur charges.
