---
title: Host provisioning format
description: Declare the first-boot format for host replacement and report installed observations.
---

Host images are opaque provider identifiers. A controller cannot reliably infer
whether an image accepts cloud-init or Ignition from its name or the distribution
currently running on the host.

## Desired replacement configuration

`Machine.spec.host.provisioningFormat` declares `CloudInit` or `Ignition` for the
desired host image. A versioned MachineConfiguration can also declare
`spec.template.host.provisioningFormat` with its `image`.

```yaml
spec:
  host:
    image: provider-specific-image-id
    provisioningFormat: CloudInit
```

For HostReplace, image and format are selected together from one configuration
version and recorded in the operation target's `input.hostImage` and
`input.provisioningFormat`. Later Machine edits do not change this frozen pair.

Resolution follows these rules:

1. A Machine image overrides a template image. Whitespace-only image values count
   as absent; selected image identifiers have surrounding whitespace removed.
2. An explicit Machine provisioning format wins. When inheriting the template
   image, its declared format is used if the Machine has no format declaration.
   A Machine image override does not inherit a different template image's format.
3. When preserving the current image, the installed observation may supply the
   format. An explicit replacement image always requires a declared target format,
   even if the current installation uses CloudInit.
4. With neither a declaration nor an applicable observation, replacement fails.
   There is no implicit CloudInit default.

The controller currently generates **cloud-init only**. A resolved Ignition
HostReplace fails before the destructive provider call. Declaring Ignition does
not add controller-driven Ignition delivery. If an image changes provisioning
format, declare the new target format explicitly rather than editing observations.

Replacement operation snapshots must include the frozen provisioning format.
Snapshots without it fail before replacement bootstrap data is generated; they
are not repaired from mutable Machine declarations or observations. Create a new
operation after declaring the intended image and format.

## Installed observations

The agent accepts an optional `ProvisioningFormat` installation-config field with
the values `cloud-init` or `ignition`. Explicit values are reported through
`Machine.status.observedProvisioningFormat`, including on pre-created Machines.
Status reporting preserves desired spec and uses the existing Machine status RBAC.

Omitted installation format remains unobserved. The agent does not infer it from
the host OS and does not manufacture a CloudInit observation for an unknown host.
Existing script/cloud-init renderers continue to leave that observation unset.

Deploy the updated CRDs and controller before agents that report explicit format
observations. Older controllers do not enforce the new replacement protection;
declaring a format is not sufficient protection while an older replacement
controller still processes operations.
