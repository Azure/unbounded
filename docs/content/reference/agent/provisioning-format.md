---
title: "Host provisioning format"
description: "Declare whether a host image expects cloud-init or Ignition data, understand how replacement resolves that declaration, and see how the agent reports what it installed."
---

The host provisioning format is a two-value declaration: an image expects
either `CloudInit` or `Ignition` data on first boot. It is a label attached to
desired host state and to the agent's installation record, not a schema for the
user data itself. This page explains why the label exists, where it is written
and read, how a `HostReplace` operation resolves it, and how to recognize the
errors that follow from getting it wrong.

The mental model is small. An image identifier is opaque. Nothing in the string
`ubuntu-24.04`, `flatcar-stable`, or a managed image resource ID reliably tells
the controller which first-boot payload the image's initramfs will consume.
The provisioning format supplies that missing fact explicitly. The controller
uses it to decide whether it can generate replacement data, and the agent can
report the format recorded for its installation.

## Purpose

A host image that consumes cloud-init reads a `#cloud-config` document, usually
from a metadata service or a provider-specific user-data field. An image that
consumes Ignition reads a JSON document in the Ignition specification. The two
formats are not interchangeable. If you hand cloud-init YAML to an Ignition
image, the image boots without the files, units, and agent configuration the
document was meant to install. The node never registers, and the failure is
visible only on the host console or in the provider's serial log.

The declaration exists so that the replacement controller can refuse that
mistake before it destroys a working host. It is most important for
`HostReplace`, which is destructive: the controller plans a new image and new
first-boot data, then asks the provider to replace the host. A wrong format at
that point would be unrecoverable if it reached the provider, because the old
host would already be gone.

The format can also be recorded by the agent after installation. When it is,
the cluster knows whether a Machine was booted with cloud-init or Ignition, even
if the Machine's desired image changes later. That observation distinguishes an
installation that recorded its format from one that predates the field.

### What the format is not

The format is not inferred from the image name. Image identifiers are opaque
provider-interpreted strings, and the API documentation says so directly. A
distribution name, a version suffix, or a gallery image name may suggest a
format to a human, but the controller does not parse those strings to choose a
payload. This is deliberate: the same distribution can be published in images
that consume different first-boot formats, and providers are free to change
image naming without changing the contract.

The format is not inferred from the running distribution either. The agent does
not inspect `/etc/os-release`, the init system, or the presence of an Ignition
binary and then manufacture an observation. An installation that does not
declare a format stays unobserved. That rule protects older installations: if
the agent guessed, every host upgraded to a newer agent would suddenly acquire
an observation that the cluster never verified.

The format is not the image identifier, the agent OCI image, or the isolation
mode. `Machine.spec.host.image` selects the operating system image that the
provider launches. `Machine.spec.agent.image` selects the OCI image used for
the node stack. `Machine.spec.isolation` selects where that stack runs. Only
`provisioningFormat` describes the first-boot payload the host image consumes.

{{< callout type="warning" >}}
The controller currently generates cloud-init only. Declaring `Ignition` on a
host that you expect the machine-ops controller to replace will make the
operation fail before the provider is called. That is intentional: the
controller will not send a payload it cannot produce.
{{< /callout >}}

## Where the format is declared and discovered

The format appears in two different places with two different jobs. In the
cluster API it is desired intent and recorded observation. On the host it is an
optional field in the agent's JSON configuration file. Keeping those roles
separate explains most of the behavior described later.

### The agent configuration file

The agent reads its configuration from the JSON file named by the
`UNBOUNDED_AGENT_CONFIG_FILE` environment variable. There is no built-in
default path. When the variable is unset, the agent falls back to the legacy
per-field environment variables such as `KUBE_VERSION`, `API_SERVER`, and
`BOOTSTRAP_TOKEN`, and those variables have no way to express a provisioning
format. An installation performed through the legacy environment path is
therefore unobserved even if it booted an Ignition image.

The field is a top-level string in that JSON document. Only the fields relevant
to this page are shown; a complete file also carries the cluster, kubelet, CRI,
and CNI sections.

```json
{
  "MachineName": "worker-1",
  "ProvisioningFormat": "ignition"
}
```

The accepted values are the lowercase strings `cloud-init` and `ignition`, or
an omitted field. The loader trims surrounding whitespace before validation, so
`" ignition "` is accepted and normalized, but `"Ignition"` and
`"CloudInit"` are not. The two schemas use different conventions: the agent
configuration uses lowercase values, while the CRD enum uses capitalized ones.

Several bootstrap paths write this file, and each uses a different path:

| Path | Supplied by | Lifetime |
| --- | --- | --- |
| Path named by `UNBOUNDED_AGENT_CONFIG_FILE` | The installer or provisioning system that starts the agent | Until the operator or installer removes it |
| `/etc/unbounded/agent/config.json` | Netboot vendor data and the Ignition variant of `kubectl unbounded machine manual-bootstrap` | Persistent on the host |
| `/tmp/unbounded-agent.json` | Cloud-init generated by the machine-ops controller for `HostReplace` | Removed by the generated command after the install finishes |
| `/etc/unbounded/agent/<name>-applied-config.json` | The agent itself, after a successful bootstrap | Persistent, with a `.sha256` sidecar |

The applied configuration is a copy of the agent configuration the running node
was built from. Its name depends on the isolation mode and the blue/green slot:
an nspawn node uses `kube1-applied-config.json` or `kube2-applied-config.json`,
a `none` node uses `none-kube1-applied-config.json`, and a container node uses
`container-kube1-applied-config.json` or `container-kube2-applied-config.json`.
The applied file is a record used for drift detection. It is not the place to
edit the format; edit the input configuration and let the next bootstrap write a
new applied copy.

The `start` command validates the loaded configuration before it bootstraps the
host, so an invalid `ProvisioningFormat` stops the bootstrap with the validation
error described in [Troubleshooting](#troubleshooting). The `preflight` command loads
the same configuration, which makes it a safe way to check a file before a host
reboots. The installer also checks only that the named file exists; it does not
inspect the format.

### Discovery order in the cluster API

For `HostReplace`, the controller resolves the image and the format from one
version selection so that the pair cannot be mixed across a configuration
change. It reads the following sources in order:

1. `Machine.spec.host.provisioningFormat`, if set.
2. `MachineConfigurationVersion.spec.template.host.provisioningFormat`, but only
   when the Machine did not set its own image and the template image is being
   used.
3. `Machine.status.observedProvisioningFormat`, if the agent reported one.
4. `CloudInit`, the legacy default for an installation with no declaration and
   no observation.

The order is not a simple list of independent fallbacks. The template is read
only when the Machine image is empty and a configuration reference is set, and
the explicit-image guard described in [Resolution rules](#resolution-rules) can
turn the observation step into an error instead of a fallback. The next sections
state the exact rules.

## Complete schema

Only a small number of fields participate in this feature. The tables below
cover every one of them, including the fields that carry the image alongside the
format. Paths include the top-level field location, either `spec` or `status`.

### Machine host specification

`Machine.spec.host` holds the per-machine desired host settings. It is the first
source of replacement intent.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `image` | `string` | No | Empty, which preserves the current image | Opaque provider-interpreted image identifier. The controller trims surrounding whitespace. A whitespace-only value is treated as absent. |
| `provisioningFormat` | `string` enum: `CloudInit`, `Ignition` | No | Empty, resolved by the rules below | Declares the first-boot format the desired image consumes. |

`Machine.spec.host` also contains the mutually exclusive host ownership fields
`netboot`, `azure`, and `external`. At most one of them may be set. Those
fields identify the host and its provider; they do not change the meaning of
`provisioningFormat`.

### MachineConfiguration host template

`MachineConfiguration.spec.template.host` is versioned configuration. Each
`MachineConfigurationVersion` receives a copy of the template, and the
replacement controller reads the copied version, not the live
MachineConfiguration. Editing the live template does not retroactively change
an existing version.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `image` | `string` | No | Empty | Opaque image identifier inherited by Machines that do not set their own image. |
| `provisioningFormat` | `string` enum: `CloudInit`, `Ignition` | No | Empty | First-boot format understood by the template image. It is inherited when the Machine image is absent and a configuration reference is set. |

The selection rule is the important part. A Machine that sets its own image
does not inherit the template format, because the template describes the
configured image, not the override. The controller does not assume that two
different images share a first-boot format.

### MachineAllocation snapshot

A MachineAllocation freezes the inputs used to launch an allocated host. The
snapshot records the provider-planned image and the configuration template that
was selected at planning time.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `status.snapshot.hostImage` | `string` | Yes | None | The provider-interpreted image the allocation planned. It becomes the Machine's `spec.host.image`. |
| `status.snapshot.configurationTemplate.host.image` | `string` | No | Empty | The template image frozen into the snapshot. |
| `status.snapshot.configurationTemplate.host.provisioningFormat` | `string` enum: `CloudInit`, `Ignition` | No | Empty | The template format frozen into the snapshot. Allocation accepts only empty or `CloudInit`. |

Allocation is a cloud-init-only path. Before it plans an offering, the
allocation controller rejects a configuration version whose template declares
`Ignition`, with this message:

```text
MachineConfigurationVersion <name> uses provisioningFormat Ignition; allocated hosts support only CloudInit
```

The Machine that the allocation creates is always written with
`provisioningFormat: CloudInit`, even though the image comes from the provider's
allocation plan. That is not an assumption about the image. It is a statement
about what this controller path can provision.

### MachineOperation target input

A `MachineOperation` for `HostReplace` stores the resolved image and format in
its target input before any provider call. The controller writes it once and
treats it as immutable, so retries reuse the same pair.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `status.targets[].input.hostImage` | `string` | No | Empty, which tells the provider to preserve the current image | The resolved provider image for the replacement. |
| `status.targets[].input.provisioningFormat` | `string` enum: `CloudInit`, `Ignition` | No | Empty for a snapshot written before the field existed | The format resolved from the same configuration selection as `hostImage`. |

The pair is written together by the machine-ops controller at target
initialization. The controller builds the provider request from the frozen
`hostImage` and uses the frozen `provisioningFormat` to decide whether it can
generate replacement user data. When the input carries a format, it does not
re-read either value from the Machine. This is what lets a retry finish the
operation it started even if somebody edits the Machine while the provider is
working.

### Machine status observation

The agent reports its own installation record in Machine status. This field is
written by the agent, not by the controller, and it is never used as desired
state.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `status.observedProvisioningFormat` | `string` enum: `CloudInit`, `Ignition` | No | Empty, meaning unobserved | The format the agent explicitly recorded for this installation. It does not override a desired replacement format. |

The agent writes this field through the status subresource with a merge patch
and optimistic locking. If another writer updates the status at the same time,
the agent retries the patch and preserves the other status fields. When the
observed value already matches, the agent skips the write.

### Agent configuration field

The agent configuration uses a different JSON schema and spelling from the CRD.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `ProvisioningFormat` | `string`: `cloud-init`, `ignition`, or empty | No | Empty, meaning legacy or undeclared | Records how the host was first booted. The loader trims whitespace and validates the value. An empty value is accepted for compatibility. |

The JSON key is capitalized because the agent configuration struct does not
rename it. Go's JSON decoder matches field names case-insensitively, so a
lowercase `provisioningFormat` key also decodes, but the documented spelling is
`ProvisioningFormat` and generated files use it. The value, not the key, is
case-sensitive: only lowercase `cloud-init` and `ignition` pass validation, and
the CRD-style `CloudInit` and `Ignition` spellings do not.

### Value spellings

The two surfaces use different spellings for the same two concepts. Use this
table when translating between a CRD manifest and an agent JSON file.

| Context | Cloud-init value | Ignition value | Empty value |
| --- | --- | --- | --- |
| `Machine` and `MachineConfiguration` CRDs | `CloudInit` | `Ignition` | Omitted or `""` |
| `MachineOperation` target input | `CloudInit` | `Ignition` | Omitted, resolved as a legacy snapshot |
| Agent JSON configuration | `cloud-init` | `ignition` | Omitted or `""` |
| `Machine.status.observedProvisioningFormat` | `CloudInit` | `Ignition` | Omitted, meaning unobserved |

There is no conversion from the CRD spelling to the agent spelling inside the
agent. The daemon maps the lowercase configuration value to the capitalized
status enum explicitly. A configuration value of `CloudInit` fails validation
instead of being normalized.

## Defaults

An empty value is meaningful at every layer, but it does not mean the same
thing everywhere. The table lists what an omitted field does in each context.

| Field | When empty | Effect |
| --- | --- | --- |
| `Machine.spec.host.image` | Always allowed | Preserve the current image. If a configuration reference is set, the template image is used instead of preserving, because an empty Machine image means "use the configured image". |
| `Machine.spec.host.provisioningFormat` | Always allowed | Do not declare a desired format. Resolution uses the template (only when the template image is used), then the observation, then the legacy default. |
| `MachineConfiguration.spec.template.host.provisioningFormat` | Always allowed | No template format. A Machine with no image and a configuration reference falls through to the observation and legacy default. |
| `Machine.status.observedProvisioningFormat` | Before the agent reports | The installation is unobserved. The replacement controller will not claim to know the installed format. |
| `MachineAllocation.status.snapshot.configurationTemplate.host.provisioningFormat` | Always allowed | Allocation proceeds and creates its Machine with `CloudInit`. Only a non-empty value other than `CloudInit` is rejected. |
| `MachineOperation.status.targets[].input.provisioningFormat` | For snapshots written before the field existed | Treat the target as a legacy snapshot and resolve the format from current intent and observations. |
| Agent `ProvisioningFormat` | Always allowed | Write no observation. An existing observation is left alone rather than being cleared. |

The legacy default is `CloudInit`. The replacement resolver applies it at its
final fallback, and the allocation controller writes it explicitly on the
Machine it creates. An empty observation never becomes `CloudInit` in status.
The distinction matters because an empty observation means "the agent did not
say", while a `CloudInit` observation means "the agent explicitly reported
cloud-init".

## Resolution rules

The replacement resolver produces one image and one format. It reads a single
configuration version so that an unpinned reference cannot change between two
reads and freeze incompatible halves. The algorithm is:

1. Trim the Machine's `spec.host.image`. If only whitespace remains, treat the
   image as absent.
2. If the Machine image is absent and `spec.configurationRef` is set, resolve
   the referenced configuration version once. Take its template image. If the
   Machine did not declare a format, take the template format as well.
3. If the Machine declares a format, use it. This includes a format copied from
   the template in the previous step; an explicit Machine declaration was never
   overwritten.
4. If no format is declared and the resolved image is non-empty, check the
   observation. An observed `Ignition` at this point is an error, not a
   fallback, because replacing an Ignition host with an explicit new image
   requires you to say whether the new image is Ignition or a deliberate
   migration to cloud-init.
5. If no format is declared and an observation exists, use the observation.
6. Otherwise, use `CloudInit`.

The controller writes the resulting pair into the operation target input before
it calls the provider. The provider request is built from that input, so later
edits to the Machine do not change an operation that is already in progress.

### Explicit declaration wins

`Machine.spec.host.provisioningFormat` is the highest-precedence source. If it
is set, the resolver returns it without consulting the template or the
observation. This is the supported way to migrate a host between formats. For
example, a host observed as Ignition can be replaced with a cloud-init image by
setting both the new image and `provisioningFormat: CloudInit` on the Machine.
The observation still says Ignition, because it describes the old installation;
the declaration describes the new one.

### Template inheritance requires an absent Machine image

The template format is inherited only when the Machine image is absent and a
configuration reference is set. The controller resolves the version, takes the
template image, and copies the template format into the resolved host state if
the Machine did not declare one. When the template sets both values, they travel
together from the same version.

This is the normal path for a fleet that shares one image. Put the image and
format in the versioned template, leave per-machine host image empty, and every
HostReplace uses the same pair:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineConfiguration
metadata:
  name: workers
spec:
  template:
    host:
      image: <provider-image-id>
      provisioningFormat: Ignition
    kubernetes:
      version: v1.34.0
```

### A Machine image override does not inherit the format

If the Machine sets its own image, the controller does not read the template at
all for replacement purposes. The template describes the configured image, and a
different image may consume a different payload. A Machine image override with
no format therefore falls through to the observation and then to `CloudInit`; it
does not silently become Ignition because the template said so.

This is the rule that most often surprises operators during a migration. If you
override the image on one Machine, set the format on that Machine too:

```yaml
spec:
  host:
    image: <new-provider-image-id>
    provisioningFormat: Ignition
```

### The observation is a fallback, not an override

When the resolved image is empty, the provider preserves the current image, and
the resolver can safely reuse the installed format. An observed `Ignition` or
`CloudInit` is returned in that case. This is how a host whose agent reported
Ignition survives a `HostReplace` that does not change the image.

The observation never overrides a declaration. If the Machine says
`CloudInit` and the status says `Ignition`, the declaration wins. That is the
intended migration path: the declaration is the target, and the observation is
the history.

### A known Ignition host needs an explicit target format for a new image

If the resolved image is non-empty and the observation is Ignition, and the
Machine declares no format, the resolver returns this error:

```text
HostReplace with an explicit image on an Ignition host requires an explicit target provisioningFormat
```

The resolver rejects the target before storing an input, so a second attempt
without a declaration produces the same error. Add either
`provisioningFormat: Ignition` for another Ignition image or
`provisioningFormat: CloudInit` for a deliberate migration, then start the
replacement again.

### Cloud-init is the legacy fallback

If there is no declaration, no observation, and no explicit-image conflict, the
resolver returns `CloudInit`. This covers installations that predate the field
and hosts whose agents were installed without an explicit format. The fallback
is a compatibility rule, not evidence that the image is a cloud-init image. If
you know the host is Ignition, declare it; do not rely on the default.

### Whitespace is removed from the image

The resolver trims surrounding whitespace from `spec.host.image` before it
decides whether the image is empty. Values that consist only of spaces, tabs, or
newlines are treated as absent. That means a YAML value such as `image: " "` is
treated as unset, so the template or observation path is used and the provider
preserves the image when no template image applies. It never becomes an image
identifier containing one space. The Azure provider trims the identifier again
when it parses the reference.

### Resolution examples

The table shows the inputs and the resolved pair. "Preserve" means the provider
is told to keep the current image.

| Machine image | Machine format | Template image | Template format | Observed | Resolved image and format |
| --- | --- | --- | --- | --- | --- |
| Empty | Empty | `template-image` | `Ignition` | Empty | `template-image` + `Ignition` |
| Empty | Empty | `template-image` | `Ignition` | `CloudInit` | `template-image` + `Ignition`, because the template declaration outranks the observation |
| `own-image` | Empty | `template-image` | `Ignition` | Empty | `own-image` + `CloudInit`; the template is not read for an image override |
| `own-image` | `CloudInit` | `template-image` | `Ignition` | `Ignition` | `own-image` + `CloudInit`, an explicit migration |
| `own-image` | Empty | None | None | `Ignition` | Error; a new image on a known Ignition host needs an explicit format |
| Empty | Empty | None | None | `Ignition` | Preserve + `Ignition` |
| Empty | Empty | None | None | Empty | Preserve + `CloudInit` |

## Examples

The examples show the host portion of a Machine. The ownership block (`netboot`,
`azure`, or `external`) is set by the path you use and is omitted where it does
not affect the format decision.

### Cloud VM with a cloud-init image

A cloud VM uses the provider's image reference and declares the format the
image's first boot expects. For the built-in Azure provider, the image is either
a full Azure resource ID that identifies a Microsoft.Compute image or gallery
image, or a `publisher:offer:sku:version` reference:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-1
spec:
  host:
    image: Canonical:ubuntu-24_04-lts:server:latest
    provisioningFormat: CloudInit
    azure:
      resourceID: /subscriptions/<subscription-id>/resourceGroups/<resource-group>/providers/Microsoft.Compute/virtualMachines/worker-1
```

The Azure provider trims the image, accepts a leading `/` only for a resource
ID that names a Microsoft.Compute image or gallery image, and otherwise requires
exactly four non-empty colon-separated parts. A malformed reference is rejected
when the provider parses the replacement plan, not when the Machine is created.

### Bare metal with an Ignition image

A bare-metal host that boots an immutable image usually receives Ignition data
from the network boot path. The Machine records the format so that a later
replacement does not assume cloud-init:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-2
spec:
  host:
    image: <bare-metal-image-id>
    provisioningFormat: Ignition
```

The `image` value is provider-specific; this page does not define its syntax.
The format is not derived from the fact that the host uses PXE or from the
image's name. If your installation delivers Ignition, say `Ignition` explicitly,
including in the MachineConfiguration template used for the fleet.

### Immutable image on an external provider

An external provider that replaces hosts through the `HostReplace` contract
uses the same fields. The provider receives the frozen image with the operation
request; the format stays controller-side and decides whether a payload can be
generated:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-3
spec:
  host:
    image: <external-provider-image-id>
    provisioningFormat: Ignition
    external:
      provider: example
      providerID: example:///hosts/worker-3
```

A replacement of this Machine will fail before the provider is called, because
the machine-ops controller cannot generate Ignition data. The declaration is
still correct, and it prevents the controller from sending cloud-init to a host
that would ignore it. To replace the host, either perform the replacement
through a system that can deliver Ignition, or declare `CloudInit` together
with a cloud-init image as an explicit migration.

### A Machine overriding a template image

Use a template for the shared defaults and declare both values on a Machine
that overrides the image:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-4
spec:
  configurationRef:
    name: workers
  host:
    image: <worker-4-image-id>
    provisioningFormat: CloudInit
```

Leaving `provisioningFormat` off this Machine would not inherit `Ignition` from
the `workers` template. The override would resolve to `CloudInit` unless the
agent had reported Ignition, in which case the explicit-image guard would make
the operation fail. Declaring the format removes both ambiguities.

### Agent configuration for an Ignition installation

The agent configuration must use the lowercase spelling. A complete file also
contains the cluster, kubelet, CRI, and CNI sections; only the fields relevant
to this page are shown:

```json
{
  "MachineName": "worker-3",
  "ProvisioningFormat": "ignition"
}
```

Place this file where `UNBOUNDED_AGENT_CONFIG_FILE` points before the agent
starts. The netboot vendor data and the Ignition variant of
`kubectl unbounded machine manual-bootstrap` write it to
`/etc/unbounded/agent/config.json`. The controller-generated cloud-init for
`HostReplace` writes its own copy to `/tmp/unbounded-agent.json` and removes it
after the install.

The generated payloads do not set `ProvisioningFormat` and `manual-bootstrap`
has no flag for it, so an Ignition installation also needs the Machine
declaration described in [Installed observations](#installed-observations).
Otherwise the installation stays unobserved, and a later explicit-image
replacement resolves to `CloudInit` and can replace the host with a payload the
image ignores.

## Validation rules

Validation is split across the API server, the agent configuration loader, the
replacement controller, and the allocation controller. Each layer enforces the
part of the contract it can see.

| Rule | Enforced by | What happens on failure |
| --- | --- | --- |
| The CRD value must be `CloudInit` or `Ignition`. | Kubernetes API server, from the enum in the CRD schema. | The create or update is rejected and the message names the field path. |
| The agent configuration value must be `cloud-init`, `ignition`, or empty, after trimming whitespace. | `ValidateProvisioningFormat`, called by `AgentConfig.Validate` and by the status reporter. | `start` stops before bootstrapping; the reporter returns an error. |
| `Machine.spec.host.image` is trimmed, and a whitespace-only image is absent. | Replacement resolver. | The image is treated as unset, so the template or preserve path is used. |
| An explicit replacement image on an observed Ignition host needs an explicit format. | Replacement resolver. | The operation target input cannot be built; the operation fails with the explicit-target error. |
| An allocated host's template format must be empty or `CloudInit`. | Allocation controller. | The allocation fails with `FailureInvalidRequest` before an offering is selected. |
| A `HostReplace` payload must be cloud-init. | Machine-ops controller, before the provider call. | The operation fails with the cannot-generate error; the provider is not called. |
| At most one of `netboot`, `azure`, and `external` may be set on a host. | CRD validation. | The Machine is rejected. This is host ownership, separate from the format. |

The API server has no opinion on the agent configuration file. The agent loader
trims the value before validation. If you write `" ignition "` in JSON, it passes
validation and is treated as `ignition`. If you write `"Ignition"`, it fails,
because the comparison is against the lowercase agent values only.

The CRD enum is also case-sensitive. `CloudInit` and `Ignition` are the only
accepted strings. A value such as `cloud-init` is rejected by the API server
even though the same string is the correct agent configuration value.

## Controller behavior for HostReplace

The machine-ops controller is the component that generates replacement data.
Its behavior is deliberately narrow: it can produce cloud-init, and it refuses
everything else before it touches the provider.

### Cloud-init is the only generated payload

`buildReplaceUserData` checks the frozen format first. If the format is anything
other than `CloudInit`, it returns:

```text
HostReplace cannot generate Ignition provisioning data: controller-driven replacement currently emits cloud-init only
```

The check runs while the operation request is being built, before the agent
configuration is assembled and before the provider request is sent. A failed
operation therefore leaves the existing host in place; after you change the
declaration, start the replacement again. This is the most important safety
property of the feature: a wrong format cannot destroy a host and then fail to
boot it.

The generated cloud-init writes the agent configuration and install script,
runs the installer, and removes the temporary files. The cloud-init document
starts with `#cloud-config`. Nothing in that path produces Ignition JSON.

### There is no controller-driven Ignition delivery

The controller does not render Ignition and does not hand an Ignition document
to a provider. If your host image requires Ignition, the first boot must be
handled by a system that can deliver it: a network boot integration, a provider
feature, or a manually generated Ignition configuration. The cluster-side
declaration still matters, because it prevents a later controller replacement
from assuming cloud-init.

This also means that declaring `Ignition` does not enable a replacement path.
It records a fact and blocks an unsafe operation. It does not add a capability.

### After an image format change, declare the target explicitly

When you change an image, the old observation describes the old image. The
resolver will not guess the new image's format from its name or from the old
host. If the old observation is Ignition and the new image is explicit, the
resolver stops and asks for a declaration. If the old observation is empty or
CloudInit and the new image is Ignition, the resolver falls back to CloudInit,
and the controller can replace the host with a payload the new image ignores.
Declare the target format on the Machine or on the template version that the
Machine uses, even when the fallback would let the operation proceed.

## Legacy operation snapshots

The target input fields were added after the operation API existed. Operations
created before the upgrade can have a target input with no format, or no target
input at all. The controller handles both shapes without pretending the pair
was frozen.

### Snapshots without a format

When `status.targets[].input.provisioningFormat` is empty, the controller treats
the target as a legacy snapshot. It resolves the format from the current
Machine declaration and observation, using the same rules as a new operation.
The image is taken from the snapshot when the snapshot has one. If the snapshot
has no input at all, the image is empty and the provider is asked to preserve
the current image.

### The snapshot image is still used

A legacy snapshot's `hostImage`, when present, is used unchanged. Editing the
Machine's image does not rewrite an operation that already has a snapshotted
image. This preserves the original intent of the operation and avoids mixing an
old image with a new format decision.

### The frozen-format guarantee is missing

A legacy snapshot has no frozen format. Only the image is frozen. If somebody
edits the Machine while the operation is running, the format recomputed on the
next retry can differ from the format the operation started with. The controller
can therefore generate cloud-init for an operation that began against an
Ignition host, or fail with the explicit-target error, depending on the edit.

Do not edit `spec.host` or the configuration reference of a Machine with an
in-flight legacy `HostReplace`. For new operations, the pair is frozen at target
initialization and the problem does not arise.

## Installed observations

The observation is the agent's statement about its own installation. It is
optional, explicit, and never inferred.

### The agent configuration field is optional

`ProvisioningFormat` is optional in the agent configuration. An empty value is
valid and means the installation predates the field or was performed by a
system that did not record one. The daemon accepts an explicit value, maps it to
the CRD enum, and writes it to `Machine.status.observedProvisioningFormat`.

The mapping is exact: `cloud-init` becomes `CloudInit`, `ignition` becomes
`Ignition`, and an empty value produces no write. The value is trimmed before
the comparison, so surrounding whitespace is harmless, but a misspelled value
is not.

### Reporting uses the Machine status subresource

The daemon reports the observation during registration, whether it just created
the Machine or found an existing one. If the Machine already exists, the agent
leaves its spec untouched and patches only the status. If the Machine does not
exist and the host is not an allocation host, the agent creates a minimal
Machine and then reports the observation. An allocation host never
self-registers a Machine; the allocation controller creates it, and the agent
only reports status for the Machine it finds.

The daemon's ClusterRole grants `get`, `update`, and `patch` on
`machines/status`, along with `create`, `get`, `list`, and `watch` on
`machines`. A pre-created Machine works with the same permissions because the
report is a status patch, not a spec update. If the status permission is
missing, the patch fails and the observation does not appear. The daemon treats
this as an error rather than a best-effort miss, so startup fails with a
`register machine` error.

The patch uses optimistic locking. A conflict with another status writer is
retried, and the patch preserves conditions and other status fields written by
that writer. When the status already holds the same value, the agent does not
write at all.

### Omitted means unobserved

An agent configuration without the field writes nothing. It does not clear an
existing observation, and it does not default the status to `CloudInit`. The
controller therefore sees an empty observation for that host and uses its own
resolution rules. This is why upgrading an agent does not rewrite history: the
absence of a declaration stays absence.

The agent also does not inspect the host OS to manufacture an observation. A
host running an immutable distribution is not automatically Ignition, and a
host with cloud-init installed is not automatically CloudInit. Only the
configuration field produces an observation.

### Legacy hosts do not get a synthetic CloudInit observation

The controller's legacy fallback is `CloudInit`, but that fallback lives in the
resolver, not in Machine status. A host installed before the field existed
remains `observedProvisioningFormat: ""` until an agent configuration explicitly
reports something. This keeps the status honest and lets you distinguish "the
agent said cloud-init" from "nobody ever said".

### In-tree renderers leave the field unset

The netboot vendor data, the controller's generated cloud-init, and the Ignition
variant of `kubectl unbounded machine manual-bootstrap` all produce agent
configuration without `ProvisioningFormat`. The field exists in the schema, but
these paths do not populate it.

For a cloud-init installation this is harmless: the resolver's legacy fallback
is cloud-init anyway. For an Ignition installation it matters. If your
provisioning path can set the field, add it to the agent configuration before
first boot; otherwise set the Machine declaration. If you do neither, the status
stays empty and a later explicit-image replacement resolves to `CloudInit`. The
controller can then proceed with a payload the image ignores, which is the
failure mode the declaration is meant to prevent.

## Deployment ordering

The feature spans a CRD field, a controller, and the agent. Roll them out in that
order.

1. Apply the updated CRDs. The Machine CRD must carry
   `spec.host.provisioningFormat` and `status.observedProvisioningFormat`; the
   MachineConfiguration and MachineConfigurationVersion CRDs must carry the
   template field. The MachineAllocation and MachineOperation CRDs must carry
   the snapshot and target-input fields.
2. Upgrade the machine-ops controller and the allocation controller. They are
   the components that read the declaration, enforce the guards, and write the
   frozen pair.
3. Roll the agents. They are the components that report the observation.

Rolling the agent first is mostly harmless but pointless: with an older CRD the
new status field is pruned, so the observation is written and lost. With an
older controller, the declaration is ignored and the enforcement gap below
applies.

### The older controller and CRD gap

A CRD that predates the field prunes it. The declaration may appear to apply
successfully and then be missing from the stored object, and the status
observation has nowhere to live. A controller that predates the field does not
enforce the explicit-image guard and does not freeze the pair. It can generate
cloud-init for a Machine that declares `Ignition`, because the declaration is
just an unknown field to that binary. The host is then replaced with a payload
its image will not consume.

This is why the declaration is not sufficient on its own. The guard exists only
in a controller that understands it. If you apply the new CRD and set
`spec.host.provisioningFormat: Ignition` but leave an older machine-ops
controller running, the API server stores the declaration and the old controller
ignores it. Treat the declaration as effective only after both the CRD and
controller rollouts are complete.

## Troubleshooting

Most failures in this feature are configuration errors with specific messages.
Start with the agent configuration file and the Machine's host state, then move
to the operation input.

### The agent configuration fails to parse

The loader wraps file and decode errors with the path:

```text
read agent config file "/etc/unbounded/agent/config.json": open /etc/unbounded/agent/config.json: no such file or directory
decode agent config file "/etc/unbounded/agent/config.json": unexpected end of JSON input
```

The first means the file named by `UNBOUNDED_AGENT_CONFIG_FILE` is missing or
unreadable. The second means the file is not valid JSON, for example because a
field was added without a comma or the file was truncated. The install script
also reports `config file not found: <path>` when the variable is set but the
file does not exist.

Validate the file without starting a bootstrap:

```bash
unbounded-agent preflight --config /etc/unbounded/agent/config.json
```

`preflight` loads and validates the same configuration and does not install
packages, write units, or modify the host. If you only want to check the JSON
syntax, `python3 -m json.tool /etc/unbounded/agent/config.json` prints the
parsed document or the decode error.

For the applied configuration, a checksum mismatch produces a verification
error instead of a JSON error. The applied file and its `.sha256` sidecar are
written together; if the sidecar is missing, the loader warns and continues, and
if the sidecar is present but does not match, the load fails. Do not edit the
applied file by hand. Fix the input configuration and re-run the bootstrap.

### ProvisioningFormat must be "cloud-init" or "ignition"

This error comes from the agent configuration validator. The usual causes are
using the CRD spelling in the JSON file, or adding a value the feature does not
define:

```json
{
  "ProvisioningFormat": "CloudInit"
}
```

Change the value to `cloud-init` or `ignition`, or remove the field. An empty
value is valid and means the installation is unobserved.

### The API server rejects the format value

A Machine or MachineConfiguration with `provisioningFormat: cloud-init` is
rejected by the CRD enum. Use the capitalized values `CloudInit` and `Ignition`
in every CRD manifest. The error names the field path, for example
`spec.host.provisioningFormat`, and lists the allowed values.

### The field disappears after apply

If `kubectl get` shows no `provisioningFormat` after you applied it, check the
installed CRD version. An older CRD does not define the field, and the API server
prunes unknown fields. Apply the updated CRDs before you set the declaration or
roll out agents that report the observation.

### HostReplace fails with the explicit-target error

The message is:

```text
HostReplace with an explicit image on an Ignition host requires an explicit target provisioningFormat
```

The Machine has an explicit image and no declared format, while the status says
the installed host is Ignition. Decide which format the new image consumes and
declare it:

```yaml
spec:
  host:
    image: <new-image-id>
    provisioningFormat: Ignition
```

Use `CloudInit` instead when you are deliberately migrating the host to a
cloud-init image. After the declaration is stored, start the replacement again;
the failed attempt did not reach the provider.

### HostReplace fails with the cannot-generate error

The message is:

```text
HostReplace cannot generate Ignition provisioning data: controller-driven replacement currently emits cloud-init only
```

The resolved format is Ignition. The controller will not produce Ignition data,
so the replacement cannot proceed through this controller. Either perform the
replacement outside the controller, or migrate the host to a cloud-init image
and declare `CloudInit`. Do not remove the declaration just to get past the
error: the controller still cannot generate Ignition, and removing the
declaration only makes the target format ambiguous.

### Allocation fails with the only-CloudInit message

The allocation template declared a format other than cloud-init:

```text
MachineConfigurationVersion <name> uses provisioningFormat Ignition; allocated hosts support only CloudInit
```

Change the template used for the allocation to omit `provisioningFormat` or set
it to `CloudInit`. Allocated hosts are a cloud-init-only path; the format on the
allocation snapshot is not a way to opt into Ignition.

### The observation stays empty

Check these causes in order:

1. The agent configuration omits `ProvisioningFormat`. The in-tree renderers do
   omit it, so an Ignition installation needs an explicit value.
2. The agent was started through the legacy environment variables rather than a
   configuration file. Those variables cannot express the format.
3. The Machine CRD predates the status field, so the patch was pruned.
4. The agent's service account lacks `patch` on `machines/status`.
5. The agent failed before registration, so it never reached the reporting step.

Inspect the stored values:

```bash
kubectl get machines.unbounded-cloud.io worker-3 -o jsonpath='{.spec.host}{"\n"}{.status.observedProvisioningFormat}{"\n"}'
```

An empty second line means the host is unobserved. An empty first line means the
Machine has no host specification at all, which is normal for a Machine the
agent self-registered.

### The declaration appears to be ignored

Check the operation's frozen input before changing anything:

```bash
kubectl get machineoperations.unbounded-cloud.io <operation-name> -o yaml
```

Look at `status.targets[].input.hostImage` and
`status.targets[].input.provisioningFormat`. If the format is present, the
operation is using the frozen pair and later Machine edits will not affect it.
If it is empty, the operation is a legacy snapshot and the format is being
recomputed from current state.

Also check whether the Machine went through the template path. A Machine that
sets `spec.host.image` does not inherit `spec.template.host.provisioningFormat`.
If the Machine image is empty and a configuration reference is set, the template
image and format are read from the same version.

### A known Ignition host still receives cloud-init

The guard only fires when the observation is Ignition, the image is explicit,
and no declaration is set. An Ignition host with no observation and no
declaration still reaches the cloud-init fallback, whether the image is explicit
or preserved. Check the observation, add an explicit declaration, and confirm
the controller version. A declaration alone does not protect a host while an
older controller is running.

## Related

- [Agent configuration]({{< relref "reference/agent/configuration" >}}) for the
  complete agent JSON schema and the file loading rules.
- [Agent isolation modes]({{< relref "reference/agent/isolation" >}}) for how
  the agent configuration selects the node stack location.
- [Container mode]({{< relref "reference/agent/isolation-container" >}}) for an
  example of an image-based installation that uses Ignition.
- [Machina CRD reference]({{< relref "reference/machina-crd" >}}) for the full
  Machine, MachineConfiguration, and MachineOperation field definitions.
- [Host operations]({{< relref "guides/operations/host-operations" >}}) for the
  operational side of replacement, reboot, and power operations.
- [Bare metal]({{< relref "concepts/bare-metal" >}}) and
  [PXE boot]({{< relref "guides/pxe" >}}) for network boot provisioning.
- [Preflight]({{< relref "reference/agent/preflight" >}}) for validating an
  agent configuration before a bootstrap.
