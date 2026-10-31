# HostUpgrade: Updating the Host OS

| Field | Value |
|---|---|
| Status | Draft for review |
| Scope | Host OS updates through MachineOperation resources |
| Audience | Maintainers, operators, and agents preparing an implementation plan |
| Repository baseline | `origin/main` at `85db0c6b4868b5a8788938e47469123a9c88fdea` |
| Date | 2026-10-22 |

## Review guide

- Start with sections 1-4 for the problem and the main design choices.
- Sections 5-6 show the proposed API and examples.
- Sections 7-13 explain how updates run, recover, and report results.
- Sections 14-16 cover updater integrations and testing.
- Section 17 lists the questions we need to settle before implementation
  planning. Section 18 explains the alternatives we considered.

## 1. Purpose and context

We need a reliable way to update a worker host's operating system. Sometimes
that means updating packages and then rebooting the host or restarting the
nspawn node. Other hosts update from an OS image, using a tool such as RAUC or
Microsoft Trident to write a new OS to an inactive partition and switch to it
on reboot.

HostReplace can be too costly when all we need is a single package update. In
GPU environments, replacing a host can also lose the node's identity and
validation state. Recovering that identity and repeating validation can keep
the node out of service much longer than the update itself requires. We need
a smaller maintenance operation that preserves identity and keeps validation
state where it is still valid after the update.

The host is the physical machine or VM and its operating system. The node is
the systemd-nspawn environment that runs Kubernetes components and workloads.
Updates run on the host, outside nspawn. They may still need a node restart
afterward so the node picks up host changes.

The proposal is a new `HostUpgrade` MachineOperation. The host agent asks the
chosen updater to do the work, follows it through any restart or reboot, and
reports the result in the CR. Unbounded coordinates the update; the updater
handles packages, images, partitions, and bootloader details.

This document describes the intended behavior and the questions still open for
review. After review, it should give an agent enough direction to prepare an
implementation plan. The feature is not implemented yet.

### 1.1 How to read this draft

In this document, "must" identifies a requirement of the proposed design.
"Should" identifies a recommendation; departing from it needs an explanation.
"May" identifies an optional feature.

The requirements are still subject to review. Section 17 lists choices we need
to settle rather than leave for an implementation agent to guess. The YAML
examples show the proposed API and do not work with the current CRDs.

### 1.2 What we have agreed on

1. Call it `HostUpgrade` so the name covers both package and image updates.
2. Offer a specific upgrade operation, without exposing arbitrary host commands.
3. Choose the backend in the MachineOperation. A backend selection in
   MachineConfiguration is not required.
4. Let a separate updater manage A/B partitions, the bootloader, and rollback.
5. Support integrations such as RAUC on hosts prepared for it, including Ubuntu,
   and Trident on appropriately prepared Azure Linux hosts.
6. Follow the update through completion. Report failures during installation,
   restart, reboot, or recovery.
7. Keep track of the same updater transaction after a restart. Each backend
   adapter defines how to resume safely without repeating completed work.
8. Let package requests select named packages, an OS-specific update channel,
   or both. For example, a supported APT security channel installs security
   updates rather than all available updates.

## 2. Goals and scope

### 2.1 Goals

- Update the host while keeping its Machine identity.
- Use one operation for package updates, image updates, and native A/B updates.
- Make it clear which updater was requested and which release was installed.
- Keep accepted updates running if the daemon restarts or the API is unavailable.
- Show whether the update is installing, waiting for activation, recovering,
  or being committed by the updater.
- Resume after reboot without installing the same update again.
- Report useful progress and errors through the CR.
- Prevent conflicting maintenance from running at the same time.
- Check backend availability and compatibility before changing the host.

### 2.2 What the first version does not cover

- Arbitrary commands, shell scripts, user-supplied executable paths, raw backend
  arguments, environment overrides, or execution hooks in MachineOperation.
- Implementing partition writers, slot management, bootloaders, or watchdogs.
- Installing an updater or creating an A/B disk layout as part of an upgrade.
- Destructive reimaging, VM recreation, or automatic fallback to HostReplace.
- Cross-distribution migration or implicit package-based major-release changes.
- Fleet rollout scheduling, maintenance windows, or selector-based fan-out.
- Automatic Kubernetes cordon/drain orchestration in the initial version.
- Generic cancellation, arbitrary transaction replay, or guaranteed rollback
  for package managers that do not support it.

An image update can move to a new OS version if the updater and the release's
compatibility rules support it. That does not give the operation permission to
switch distributions or change persistent storage.

## 3. How this fits the current code

These findings describe the repository revision listed at the top of this
document. They explain the changes this design will need.

| Current behavior | Where to find it | What this design needs |
|---|---|---|
| MachineOperation is cluster-scoped with a fixed operation enum. | `api/machina/v1alpha3/machineoperation_types.go:21-26`, `:51-85` | Add the operation and typed fields through the API generation workflow. |
| The host daemon registers NodeReboot, AgentUpgrade, and AgentReset. | `cmd/agent/internal/daemon/controller.go:90-99` | Add host-upgrade execution to the host daemon, outside nspawn. |
| Local dispatch only passes operation name, kind, and string parameters. | `pkg/agent/daemon/machineoperation.go:109-122` | Pass typed upgrade inputs and UIDs to the handler. |
| The local watch excludes InProgress operations. | `pkg/agent/daemon/machineoperation.go:134-150` | Startup recovery and active-operation reconciliation are required. |
| AgentUpgrade records a pending signal before restarting the daemon. | `cmd/agent/internal/daemon/controller_machineoperation.go:148-164` | Use a similar recovery principle, with a separate journal for OS updates. |
| The external controller can fail an unsupported Host-prefixed kind. | `internal/machineops/controller.go:123-126`, `:382-384` | Define ownership before introducing HostUpgrade; external controllers must ignore it. |
| External conflict ordering excludes kinds unsupported by that provider. | `internal/machineops/resumable.go:402-413` | Coordinate upgrades and power operations across controllers. |
| Local installation locks belong to a process. | `cmd/agent/internal/installstate/lock.go:20-37` | Keep other maintenance blocked even if the daemon exits while the updater continues. |
| Existing status contains per-target input, attempts, stages, and conditions. | `api/machina/v1alpha3/machineoperation_types.go:285-343` | Extend per-target state instead of creating a separate operation lifecycle. |
| Existing completion helpers set Completed=False on failure. | `pkg/agent/daemon/machineoperation.go:237-248` | Keep this convention; use phase to tell whether an operation has finished. |
| The CLI returns an error for a Failed operation. | `cmd/kubectl-unbounded/app/machine_operation_wait.go:53-55` | Extend progress presentation while retaining failure exit behavior. |

Tests check that a fresh read of terminal status prevents dispatch
(`pkg/agent/daemon/machineoperation_test.go:93-121`) and that unsupported local
kinds leave phase unchanged (`:124-167`). These tests cover routing, not recovery
of an OS update after a restart.

### 3.1 A package manager on disk does not always mean packages can be updated

The agent already checks whether a host is image-managed before selecting APT,
TDNF, or DNF for prerequisite installation
(`pkg/agent/phases/host/os.go:107-158`). Azure Container Linux is an important
example: it can have TDNF installed even though its OS files are read-only
(`:111-119`). Tests check that the image-managed host does not select package
installation, while a writable host with the same tools selects TDNF
(`pkg/agent/phases/host/os_test.go:123-138`).

HostUpgrade needs the same care. It must check how the host is meant to be
updated, not just its distribution name or installed executables. The code that
chooses the node rootfs image is not a list of supported host updaters.

### 3.2 Existing documentation needs a follow-up

`designs/lifecycle-operations.md:21-26` assigns standalone HostHardReboot to
provider/BMC controllers and NodeReboot to the agent. Its definition of host
operations is currently limited to power and replacement (`:7-10`); this proposal
intentionally extends that vocabulary to OS maintenance.

The public host guide describes agent-initiated
HostReboot and HostPowerOff for SSH-joined hosts
(`docs/content/guides/operations/host-operations.md:30-41`), while the agent
handler registration cited above contains neither. For this design, standalone
power operations keep their existing owners. The agent can reboot gracefully as
part of HostUpgrade. The implementation plan should call out the separate
documentation question about standalone reboot on SSH-joined hosts.

## 4. What HostUpgrade means

### 4.1 HostUpgrade versus HostReplace

| Operation | Purpose |
|---|---|
| HostUpgrade | Update the existing host while keeping its Machine identity and the persistent data the host layout promises to preserve. |
| HostReplace | Recreate or reprovision a host, potentially rebuilding its OS disk and bootstrapping the agent and node. |
| AgentUpgrade | Replace the host-side Unbounded agent binary. |
| NodeReboot | Restart nspawn without replacing its rootfs. |

HostReplace is defined as out-of-band VM replacement and agent reinstallation in
`api/machina/v1alpha3/machineoperation_types.go:83-85`; physical-host reimaging is
also described in `designs/lifecycle-operations.md:24`.

An A/B update belongs under HostUpgrade when the updater preserves identity and
persistent data. Repartitioning or wiping shared data is still a destructive
operation, even if two OS partitions are involved. HostUpgrade must reject that
path rather than quietly turn into HostReplace.

### 4.2 Backend selection

Each operation must name its backend in `spec.hostUpgrade.backend`, and that
choice cannot change after creation. The initial candidates are `APT`, `DNF`,
`TDNF`, `RAUC`, and `Trident`. Each adapter needs a tested list of supported
versions and host setups.

The design must also leave room for Ubuntu Core, Mender, and OSTree-based
systems. They may need their own adapters in the Unbounded agent to handle
starting updates, resuming them, rebooting, confirming health, and rolling back.
They are future integration candidates. Installing one on a host does not
automatically make it supported by Unbounded.

There is no `Auto` backend in this proposal. If the requested backend is missing
or unsuitable, the operation fails; the agent must not choose another updater.
The host must already have the updater, configuration, trust roots, services,
and disk layout it needs.

Examples of possible pairings, once the adapters have been tested:

| Host installation | Possible backend |
|---|---|
| Mutable Ubuntu/Debian | APT |
| RAUC-provisioned Ubuntu | RAUC |
| Mutable Fedora or supported CentOS release | DNF; YUM would need its own tested adapter |
| Mutable Azure Linux | TDNF |
| Trident-provisioned Azure Linux | Trident |
| Azure Container Linux or another image-managed variant | A tested native updater; TDNF being installed is not enough |

Check OS versions, services, disk layout, and update format before changing the
host. If a check fails, report the problem. An upgrade request does not ask the
agent to install or reconfigure an updater.

## 5. Proposed request API

Add `operationKind: HostUpgrade` and typed fields under `spec.hostUpgrade`.
Keep upgrade inputs out of the existing string `parameters` map so we can
validate them properly.

| Field | Meaning |
|---|---|
| `backend` | Required supported backend name. |
| `packages` | Optional package names and an OS-specific update channel. An empty object updates all eligible installed packages from the host's configured default sources. |
| `releaseRef` | Name of an administrator-published image release, instead of `packages`. We still need to choose release storage; see section 17. |
| `rebootPolicy` | `IfRequired` by default; also `Always` or `Never`. |
| `restartNode` | Restart nspawn after the successful update/check. Defaults to false; a host reboot also satisfies this request. |

The caller chooses the updater, what to update, and whether a reboot is allowed.
The adapter handles the updater's internal activation and commit steps. Recovery
timing and native rollback settings belong in administrator-managed agent/backend
configuration, rather than in each operation.

### 5.1 Validation

- Require `machineRef`; reject `machineSelector` for HostUpgrade initially.
- Require `hostUpgrade` for HostUpgrade and reject it for other operation kinds.
- Require exactly one of `packages` or `releaseRef`, directly under `hostUpgrade`.
- Permit `packages` only for package backends, and `releaseRef` only for
  matching native release backends in the initial API.
- Check package names and channel support before changing the host. Reject
  unsupported channels rather than silently use the default sources.
- Check that the backend can honor the requested reboot policy. In particular,
  Never is not a request to stage an update for later reboot.
- Reject upgrade inputs in `parameters`, including arbitrary updater options.
- Do not allow edits to fields that affect execution after creation. Retention
  settings can follow existing rules without changing the requested update.
- Check request structure at admission, then check host and backend compatibility
  before execution.
- Track the Machine UID, not just its name. Deleting a Machine and creating a
  new one with the same name must not attach an old update to the new Machine.
- Do not submit work for an operation being deleted. Check identity and deletion
  status directly with the API immediately before submission.

### 5.2 Package selection and update channels

Package requests have two optional fields:

| Field | Meaning |
|---|---|
| `packages.channel` | Select a supported backend/OS-specific update stream or filter, such as security updates. Omit it to use the host's configured default sources and policy. |
| `packages.names` | Limit direct updates to these installed packages. Omit it to update all eligible installed packages. If provided, the list must be nonempty. |

These answer different questions: the channel chooses which updates are eligible;
the names choose which installed packages to update. They can be used separately
or together. With both present, update the named packages only from the selected
channel, plus dependencies allowed by the backend's documented policy.

For example, `channel: security` on a supported APT setup means install only
security updates, not every available update. It is not a universal Security
mode that all package managers must implement. Another OS may expose a different
named channel, release stream, or native advisory filter. Each adapter documents
which channel names it supports and how they map to that OS's update mechanism.

Channels must resolve to adapter-supported selection rules or administrator-managed
profiles for already configured sources. They are not repository URLs, arbitrary
flags, or permission to add repositories. An APT security channel needs a reliable
way to identify security updates for the installed release; it must not guess
from package names. If the host lacks the required source configuration or
metadata, reject the request before changing packages.

Use `packages: {}` for the ordinary update-all request. Respect native holds,
pins, and exclusions. Do not change repositories, remove holds, move to another
distribution release, or accept arbitrary updater flags.

Named packages must already be installed. Reject unknown or uninstalled names;
installing new software is outside this operation's initial scope. Package names
are literal names, not shell patterns, version expressions, or native options.
Version pinning can be considered separately later.

Each adapter needs clear rules for dependencies, package removals, and existing
configuration files. A named update can require changes to other packages, so
report those changes too. For a restricted channel such as security, document
whether required dependencies can come from other configured sources. If the
transaction cannot follow the channel's rules, fail rather than broaden the
request into an unrestricted upgrade. The recommendation for the first version
is to reject updates that require removals. Review the exact commands and API
calls for each backend before implementation.

Package repositories and channel contents change over time, so a package request
does not name one fixed OS image. Save the selected channel/profile and relevant
configuration identity, along with the transaction and package versions or native
history where available. Once the operation starts a transaction, follow that
transaction instead of repeatedly checking for and installing more updates.
A new operation requests a new repository check.

### 5.3 Native release targets

A release must provide the inputs its updater needs, with a stable identity:

- RAUC: a bundle location, expected digest, and the systems it is compatible with.
- Trident: a versioned, validated update Host Configuration with fixed identities
  for the images it uses.

We have not yet chosen where release definitions live. A new cluster-scoped
release resource and a versioned manifest reference are both options. Reviewers
need to choose the storage API and decide who can publish releases before
implementation planning. The examples use release names without assuming
either option already exists.

Before submitting an update, save the resolved release identity and version,
the digest of its normalized inputs, the artifact identity, and the configuration
needed to run it. A name, URL, mutable tag, or resourceVersion alone is not
enough. Keep enough information on disk to resume even if someone edits or
deletes the release definition.

Also record the local updater configuration and executable identity needed for
recovery. A configuration change must not silently redirect an active operation
to a different update.

## 6. Examples

These examples show the proposed API. Release definitions and the exact status
fields still need review, as described in section 17.

### 6.1 Ubuntu security updates with reboot only if needed

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-ubuntu-worker-01
spec:
  machineRef: worker-01
  operationKind: HostUpgrade
  hostUpgrade:
    backend: APT
    packages:
      channel: security
    rebootPolicy: IfRequired
  ttlSecondsAfterFinished: 604800
```

The adapter refreshes metadata and installs eligible security updates using the
host's supported APT security selection rules. It does not install all available
non-security updates. It waits for the package manager's result, then checks
whether a restart is needed. If this host cannot support the security channel,
the request fails rather than falling back to an unrestricted upgrade.
If the host is already current, the update succeeds without changing packages.
The OS release string may stay the same after a package update, so it cannot
be the only evidence of success.

To update all eligible packages from the default configured sources instead,
use `packages: {}` and leave the reboot policy unchanged.

### 6.2 Named package updates followed by an nspawn restart

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-and-refresh-node
spec:
  machineRef: worker-02
  operationKind: HostUpgrade
  hostUpgrade:
    backend: DNF
    packages:
      names:
        - openssl
        - kernel
    rebootPolicy: IfRequired
    restartNode: true
```

This updates the named installed packages and their required dependencies using
the host's default sources. It asks for a node restart afterward. If the update
needs a host reboot, rebooting takes care of the node restart too. The nspawn
rootfs is kept. Package names are illustrative and must match the host's actual
installed packages.

### 6.3 RAUC-managed Ubuntu A/B update

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-rauc-worker-03
spec:
  machineRef: worker-03
  operationKind: HostUpgrade
  hostUpgrade:
    backend: RAUC
    releaseRef: ubuntu-host-2026-10
    rebootPolicy: IfRequired
```

The host must already be set up for RAUC, including its slots, trusted signing
keys, and boot fallback. RAUC installs the update. Unbounded follows its progress,
coordinates the reboot, checks that the expected deployment booted, and works
with RAUC's health-confirmation process. Its boot fallback and recovery timing
come from the host's configured integration, not per-operation switches. A/B
updates require a reboot, so this request would be rejected with Never.

### 6.4 Trident-managed Azure Linux update

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-trident-worker-04
spec:
  machineRef: worker-04
  operationKind: HostUpgrade
  hostUpgrade:
    backend: Trident
    releaseRef: azure-linux-host-2026-10
    rebootPolicy: IfRequired
```

The release provides an approved Trident update configuration. Before changing
the host, the adapter checks that it describes an allowed runtime or A/B update,
not a clean install or a rebuild of the host.

### 6.5 A failed update with a successful rollback

```yaml
status:
  phase: Failed
  message: Target deployment did not pass health verification; previous deployment recovered
  targets:
    - machineRef: worker-03
      phase: Failed
      stage: RollingBack
      hostUpgrade:
        backend: RAUC
        releaseIdentity: ubuntu-host-2026-10
        outcome: RolledBack
        updateApplied: true
        activationRequired: HostReboot
        targetBootVerified: true
        nativeCommit: NotCommitted
        rollback: Succeeded
        hostRecovered: true
```

The upgrade failed, but the host recovered. Report Failed even though the old
OS is healthy again. The stage can show the last action; phase and outcome
show the final result.

## 7. How the pieces fit together

A **backend** is an updater choice, such as RAUC or APT. An **adapter** is the
Unbounded code that talks to it. The **native updater** is the tool or service
that actually changes the OS. For image updates, a **deployment** is the OS
version/image installed in a slot or selected for boot.

```text
MachineOperation
      |
      v
Host agent reconciler ---- control-plane maintenance ownership/observer
      |
      v
Durable host-upgrade supervisor and journal
      |
      v
Typed backend adapter
      |
      v
Native package manager / RAUC / Trident
      |
      v
Native deployment, boot, commit, and fallback mechanisms
```

| Unbounded handles | The updater or host provisioning handles |
|---|---|
| Resolve the request and check it | Set up the host layout and updater configuration |
| Prevent conflicting maintenance | Run the OS update |
| Submit and track the update | Write packages, images, or inactive slots |
| Save enough state to resume | Keep the updater's own transaction and deployment state |
| Decide who requests the reboot | Select the boot deployment and handle fallback |
| Check agent and node recovery | Report installation, deployment, and commit results |
| Report the final outcome | Carry out supported rollback |

The agent talks to the updater from the host, outside nspawn. Restarting the
daemon must not stop an update that the backend can keep running. Package
updates need a separate systemd-supervised runner that is independent of the
daemon and nspawn services. If a native updater already runs as a persistent
service, use that service rather than build a second transaction engine.

### 7.1 What an adapter needs to do

Each adapter connects the common workflow to a particular updater:

| Step | What it does |
|---|---|
| InspectCapabilities | Check the updater version, host layout, supported inputs, and available recovery features. |
| ValidateAndResolve | Check the release and save the exact inputs without changing the OS. |
| BeginOrRecover | Start the update, or find and resume tracking the one already started. |
| Observe | Report progress, errors, transaction identity, and any required restart. |
| Activate | Switch to the update, with exactly one owner for any reboot. |
| ConfirmHealthy | Confirm the new deployment after the agreed health checks, or observe the updater's confirmation. |
| RecoverOrRollback | Follow native fallback or request rollback when the updater supports it. |

These describe behavior, not a fixed Go interface. Prefer a structured updater
API where one is available. A fixed, validated CLI call is also acceptable.
Building shell commands from user input is not.

Support depends on the installed updater and host configuration. An API with
an update method does not, by itself, prove that we can resume after a crash or
recover from a failed boot. Those paths need to be verified.

## 8. Running the update and deciding when it is done

Retain Pending, InProgress, Complete, and Failed as top-level phases. Proposed
target stages are:

```text
Pending:     WaitingForMaintenance -> Resolving
InProgress:  Updating -> AwaitingActivation -> Activating
             -> WaitingForRecovery -> Confirming
             -> RollingBack (when required)
Terminal:    Complete or Failed
```

Backends can skip stages they do not need. Before an action that could leave us
unsure whether it started, save enough state to recover that uncertainty. After
a crash, check the updater's actual state rather than assume that a saved stage
proves the action finished.

### 8.1 When to restart or reboot

Activation means making the installed update take effect. That might require
no action, a node restart, or a host reboot. The adapter handles the native steps;
the request uses the simpler `rebootPolicy` and `restartNode` fields.

| Policy | Behavior |
|---|---|
| IfRequired | Allow a host reboot when the updater needs one. This is the default. |
| Always | Reboot after a successful update/check, even if the updater does not require one. |
| Never | Only run an update the backend can complete without rebooting. Reject an incompatible request before making changes. |

The adapter reports `None`, `NodeRestart`, `HostReboot`, or `Unknown` as the
activation requirement. Unknown does not mean no action is needed. If it cannot
determine what is needed or honor the requested reboot policy, reject the request
before changing the host when possible. Never requires a reliable preflight check
or native guarantee that the update can finish without rebooting. If the backend
cannot provide that, Never is unsupported for that request.

If the activation requirement remains unknown, or a reboot turns out to be
required despite a Never preflight check, report the failure and outstanding
restart requirement. Do not override Never, claim the update is fully applied,
or release ownership while native work might still be active. A reboot discovered
after installation is allowed under IfRequired and proceeds through normal
recovery.

`restartNode: true` asks for an nspawn restart after the successful update/check.
A backend-required node restart is also performed even if the field is false;
false means no extra restart was requested. A host reboot satisfies the node
restart too. Never restricts host reboots, not node restarts.

With IfRequired, an already-current host needs no restart unless the adapter
finds outstanding activation. Always still requests a reboot, and
`restartNode: true` still requests a node restart, even if no new update was
installed. Existing reboot-required state must be considered too: Never cannot
report a fully recovered update while a required host reboot remains outstanding.

Never is not "stage now, activate later." The first version does not offer that
workflow. It needs more design work: when does the first operation finish, who
owns the staged update, and how does a later operation activate exactly that
update?

### 8.2 One owner for reboot and boot confirmation

Choose one component to request each reboot and save that choice before calling
anything that might reboot the host. Prefer letting Unbounded request the reboot
when the updater supports that mode reliably. Otherwise, let the updater reboot
and follow its progress, after checking that this respects rebootPolicy. A backend
that can reboot automatically cannot honor Never unless it offers a verified
non-rebooting update path. A lost connection or exited client must not, on its
own, cause another reboot or another submission.

Image updaters may need a confirmation that the new OS booted successfully.
Committing the deployment means accepting it as the working OS. Agree on one
owner for this confirmation too. If another service commits automatically before
Unbounded checks health, we need to coordinate or change that setup. We cannot
promise rollback after another service has already closed that window.

A graceful reboot is part of HostUpgrade itself. Do not create a separate
provider/BMC HostHardReboot while holding the upgrade's maintenance claim: the two
operations could end up waiting on each other.

### 8.3 What counts as success

An image or A/B update is Complete only when:

1. The updater reports successful installation, or can prove the exact requested
   deployment is already installed.
2. The expected deployment is actually running. A new boot ID alone is not enough.
3. Any required restart or reboot has happened, rather than just been requested.
4. The host agent is back with the same Machine and operation identity.
5. The Node is Ready with fresh evidence from after activation. An old Ready
   condition is not enough. The adapter and recovery checks need a tested way
   to combine agent observations, Node/lease freshness, and action timestamps.
6. Any required updater confirmation or commit has succeeded, with evidence
   from the updater.

Package updates need a successful package-manager result, including a no-op
when already current, plus any required restart and recovery. They do not need
an image-slot commit if the backend has no such concept.

Each package adapter needs its own restart-detection rules. Ubuntu's
reboot-required file cannot serve as the rule for every OS.

## 9. A/B updates and rollback

In an A/B update, the native updater prepares the other OS slot, boots it on a
trial basis, and either accepts it or falls back. Unbounded must use the updater
for these steps. It must not write partitions, change bootloader variables, or
build its own fallback logic.

Where the host integration promises automatic rollback, it needs to work even
if the new OS cannot start the Unbounded agent. Before supporting that setup,
verify how it falls back, how many boots it tries, how long it waits for health
confirmation, and what happens if boot hangs. The agent/backend's configured
recovery timing needs to fit those native deadlines.

Rollback behavior comes from the native updater's administrator-managed
configuration. There is no per-operation rollback policy in the first version.
The adapter validates the host's configured guarantees before starting, uses
supported native recovery, and reports whether rollback is available and whether
it happened. Package backends must not promise rollback they cannot provide.
The request cannot disable native boot protection.

A rolled-back update is Failed with reason RolledBack. Report separately if
rollback fails or we cannot tell whether it worked. Do not keep cycling through
install, reboot, and rollback.

### 9.1 What needs to survive a slot switch

The host layout needs a clear answer for where these live and how both OS slots
can use them:

- Machine identity and agent credentials.
- The upgrade journal and maintenance ownership record.
- Updater state and information about the previous deployment.
- The nspawn rootfs, container/runtime data, and required persistent host data.
- An agent in both deployments that can read the journal and resume recovery.

Keeping the old root partition is not enough if the new OS makes irreversible
changes to shared data. A release that promises automatic rollback must check
that the old OS can still use that data. Keep the previous deployment until the
agreed commit process allows the updater to remove it.

## 10. Resuming safely after failures

Keep an on-disk record of the operation, called the upgrade journal. A proposed
location is `/var/lib/unbounded/agent/operations/<operation-uid>/`, on storage
that survives the host's actual update layout.

The journal must record:

- Schema version, Machine UID, operation UID, and request digest.
- The chosen backend, saved release inputs, artifact identity, and relevant
  configuration identity.
- The updater's transaction ID, if it has one, or enough evidence to match the
  operation to the updater's transaction and deployment.
- Which supervisor runs the work, whether submission was about to happen, and
  what result the updater reported.
- The previous and expected deployment, boot ID, reboot/confirmation owner,
  effective administrator-managed recovery settings, and stage deadlines.
- Whether the result reached Kubernetes, plus references to retained diagnostics.

Journal writes needed for recovery must be atomic and survive a crash. Save
the updater's result locally before relying on a Kubernetes status write. Keep
this separate from installation-mode state, which intentionally avoids saving
stage checkpoints (`cmd/agent/internal/installstate/store.go:26-32`).

### 10.1 What happens when something goes wrong

| Event | What to do |
|---|---|
| Reconcile runs again | Check the same transaction; do not start another update. |
| Daemon restarts | Reconnect to the updater or its supervisor and resume reporting. |
| API is unavailable | Continue accepted work using the saved policy and keep the result for later reporting. |
| Updater accepted the request but the response was lost | Check its state before deciding whether another submission is safe. |
| Update fails | Save the error and recovery state. Do not automatically run the update again. |
| Host reboots unexpectedly | Follow the backend's recovery rules. Reboot does not prove installation succeeded. |
| Expected reboot disconnects the client | Resume from the saved handoff and check which deployment booted. |
| Final status write fails | Retry publishing the saved result without repeating the update. |
| Journal is missing or unreadable | Report that recovery is uncertain. Phase alone cannot prove success. |
| Updater state is unclear | Report OutcomeUnknown and block conflicting work while the update might still be active. |
| Machine or operation is recreated with the same name | Keep the old work and result tied to the old UID. |

There is always a difficult failure window: the updater may accept a request
just before the agent crashes, before the agent saves the response. Each adapter
must explain how it handles that window and when resubmission is safe. We cannot
promise exactly-once host changes across every power failure. A progress stream
or completion signal is not necessarily a record we can recover later.

The first version has no user-controlled retry loop for failed updates. Retrying
a status read, connection, or result write is fine. Running a failed update
again normally needs a new operation, after the updater has recovered and the
previous operation has released ownership. In particular, do not try to repair
an interrupted package database by blindly running another upgrade.

## 11. Keeping maintenance operations from overlapping

### 11.1 Which component handles the operation

The host agent handles HostUpgrade. Provider/BMC controllers must ignore it
before applying generic checks for Host-prefixed operations. They still need
to respect an upgrade's maintenance claim, even though they do not run upgrades.

### 11.2 Shared maintenance ownership

An upgrade must not overlap another HostUpgrade, NodeReboot, AgentUpgrade,
AgentReset, node repave, HostHardReboot, HostPowerOff, or HostReplace. Power-on and
other recovery actions also need an explicit recovery decision before bypassing
the current owner.

A shared maintenance claim records which operation owns a Machine. Acquiring it
must be atomic so two controllers cannot both win. It needs an owner UID and a
generation that lets us reject stale work or status writes, often called
fencing. We still need to choose how to represent the claim in Kubernetes. Every
component that can run conflicting maintenance must participate.

- Checking a list of older operations is not enough to prevent a race.
- A missed heartbeat or expired Lease must not allow conflicting work. The
  updater could still be writing while the agent is disconnected.
- A local lock needs to live with the surviving supervisor or an equivalent
  updater guard, rather than just the daemon process.
- Keep ownership across reboot. Restore local protection before other lifecycle
  work can start.
- Failed status does not mean the updater stopped. Keep the claim while an
  update or rollback might still be running.
- Check UIDs and ownership generation when writing status. A delayed writer
  must not overwrite a newer owner's state or a fenced operation's result.
- Before a manual override, establish that the updater stopped or was fenced.
  An expired claim is not proof of either.

The implementation plan needs to explain how claims are acquired, recovered,
released, and handled during deletion or manual recovery. A mutex used only
inside HostUpgrade will not protect it from the other controllers.

### 11.3 Workload preparation

For the first version, the caller or a separate rollout controller cordons and
drains the node. HostUpgrade does not delete the Node or automatically uncordon
it. Reboots, nspawn restarts, and package-managed service restarts can interrupt
workloads. Reboot policy controls host reboots, not all service restarts or other
disruption during installation.
Automatic drain would need its own rules for eviction and recovery.

## 12. Reporting progress and errors

Add upgrade details to the matching `status.targets[]` entry. Keep the top-level
phase and message as the summary for this single-machine operation.

| Status information | Purpose |
|---|---|
| Backend and native version | Explain the actual implementation used. |
| Saved input identity/digest | Show exactly what was submitted. |
| Package channel, requested names, and changed packages | Show which update stream was used and any dependency changes beyond the named packages. |
| Updater transaction/deployment identity | Match the running update to its recovery state. |
| Stage and last progress time | Show what is happening without copying every log line into status. |
| Recovery deadline | Show how long the configured integration is allowing for restart, boot checks, and commit. |
| Update result and native error code | Separate installation failure from activation failure. |
| Activation requirement and reboot owner | Explain whether activation is outstanding or underway. |
| Previous, target, and booted deployment | Prove the expected OS actually booted where meaningful. |
| Native commit, rollback, and host recovery | Distinguish a successful update from a failed update that recovered. |
| Whether work is active or unknown, and who owns it | Show when maintenance is still blocked even after a reported failure. |
| Limited diagnostic tail and log reference | Help diagnose errors without storing full logs in etcd. |

Suggested reasons include InvalidSpec, BackendUnavailable, UnsupportedHost,
IncompatibleTarget, UnsupportedPolicy, UpdateFailed, ActivationFailed,
RecoveryTimedOut, CommitFailed, RolledBack, RollbackFailed, OutcomeUnknown,
NoChanges, and Succeeded. Include enough updater context to explain an error,
without exposing credentials or unlimited output.

Keep the existing Completed condition behavior. Add conditions for individual
stages where useful, and distinguish "we do not know" from "this failed." Once
the operation finishes, keep its result stable. Diagnostics and cleanup state
can still be updated, but finding that work continued after a reported failure
does not turn Failed into Complete.

On startup, look for local journals and unfinished CRs. Also keep checking
finished operations when their updater work might still be active or needs
cleanup. The current local dispatch skips terminal operations
(`pkg/agent/daemon/machineoperation.go:105-107`); that must not prevent recovery
checks or allow a second update to start.

### 12.1 Different timeouts mean different things

Recovery timing is administrator-managed agent/backend configuration, not an
operation field. Different updaters and host setups can have different budgets;
there is no universal 900-second request default. Validate the configuration
against native boot/fallback deadlines, then save the effective recovery budget
and deadline with the operation so later configuration changes do not restart
the clock or shorten an in-flight update's recovery window.

- **CLI wait timeout:** stop waiting in the client. The update continues.
- **Update progress timeout:** report stalled work. Do not kill a package manager
  by default. Any policy that stops it needs a backend-specific safe-stop design.
- **Recovery timeout:** start the clock when the activation handoff is saved to
  disk. It covers boot, health checks, and required commit. Do not reset it on
  each reconcile or daemon restart. Account for multiple reboots if the updater
  needs them.
- **Host unavailable:** a control-plane observer can report that the host is not
  reporting, or that recovery timed out. It cannot assume installation or
  rollback succeeded, or that maintenance ownership is safe to release. Its
  status writes follow the same ownership and fencing rules as the agent's.

## 13. Keeping the upgrade API constrained

HostUpgrade lets a caller request a privileged host change. Kubernetes RBAC on
MachineOperation does not distinguish operation kinds or backends on its own.
Admission rules and release-publishing permissions need to enforce who can
request which changes.

- A backend name selects a registered adapter, never an executable path, plugin
  URL, or shell command.
- Callers cannot supply arbitrary flags, environment, scripts, or updater hooks.
- Update artifacts must pass the host's trust and compatibility checks. A digest
  identifies content; it does not establish that the content is trusted.
- Requests cannot disable signature checking or replace trusted signing keys.
- Release and configuration lookups must stay within explicitly allowed scopes.
  Keep credentials in native authentication or scoped Secret references, not in
  status, logs, or public resume tokens.
- Publishing a release can introduce privileged content. Keep that permission
  separate from permission to select an approved release.
- Check Trident configurations against an update-only policy. Putting an
  unrestricted configuration behind a reference does not make it safe.
- Local administrator configuration can limit allowed backends, even though the
  operation itself selects the backend.

## 14. Integrating RAUC and Trident

The upstream interfaces below look suitable for adapters. They show what we can
integrate with, but we still need to verify recovery and boot behavior before
declaring a version supported.

### 14.1 RAUC

At upstream revision `f54d2b1d71a7cc551df487116cb34790f164570b`, the
[Installer D-Bus interface](https://github.com/rauc/rauc/blob/f54d2b1d71a7cc551df487116cb34790f164570b/src/de.pengutronix.rauc.Installer.xml)
provides InstallBundle, InspectBundle, GetSlotStatus, GetPrimary, Mark, progress
and boot-slot properties, and a Completed signal. These give us structured ways
to start an install and follow its progress.

Before supporting a RAUC version, verify:

- How to tell whether an install was accepted after losing the response.
- How to find the same update after a missed signal or service restart.
- How bundle compatibility and signature checks work.
- How activation, boot fallback, and mark-good confirmation are coordinated.
- Which options that installed version actually supports.

An installation Completed signal does not prove the new OS booted successfully
or was confirmed healthy.

### 14.2 Trident

At upstream revision `aadc65e7c51f8c08fe446637bb8748f9777cf547`:

- The [update service](https://github.com/microsoft/trident/blob/aadc65e7c51f8c08fe446637bb8748f9777cf547/proto/trident/v1/update_service.proto)
  declares Update, UpdateStage, and UpdateFinalize.
- The [commit service](https://github.com/microsoft/trident/blob/aadc65e7c51f8c08fe446637bb8748f9777cf547/proto/trident/v1/commit_service.proto)
  declares Commit.
- The [servicing protocol](https://github.com/microsoft/trident/blob/aadc65e7c51f8c08fe446637bb8748f9777cf547/proto/trident/v1/servicing.proto)
  represents progress/completion, caller-managed versus Trident-managed reboot,
  image hashes, and servicing kinds including runtime, A/B, and clean install.

Trident's [README](https://github.com/microsoft/trident/blob/aadc65e7c51f8c08fe446637bb8748f9777cf547/README.md)
describes a Host Configuration that can control storage, networking, users,
and custom hooks. Because it can do much more than update the OS, we need to
limit what HostUpgrade accepts.

Before supporting a Trident version, read its implementation, tests, and
documentation. Verify supported connections and versions, what happens when a
connection drops, how to recover state, how configuration is checked, and what
stage, finalize, commit, and rollback actually do. The proto method names do
not prove those behaviors. A native service's ability to perform a clean install
does not mean HostUpgrade may request one.

## 15. Deleting operations and upgrading Unbounded

Deleting a CR must not kill an active updater or lose track of its maintenance
ownership. A finalizer owned by the upgrade integration is the recommended
approach. If work has not been submitted, deletion stops it. If the updater has
accepted it, continue observing until it is safe to release ownership. We still
need to choose the finalizer owner in section 17.

Forcibly removing the CR or finalizer does not stop native work. Local recovery
records must still prevent overlap and retain the result of that orphaned work
for diagnosis.

Keep local results until their final status is published, unless an explicit
policy handles orphaned work. Set limits for logs and journals, but do not remove
information needed by a running or uncertain transaction. CR TTL starts after
the operation finishes, following existing behavior. TTL deletion still cannot
bypass active cleanup or ownership protection.

Use `make generate` to update CRDs and deepcopy code for the new API fields; do
not hand-edit generated files. Deploy agent and controller support for routing
and shared maintenance ownership before enabling HostUpgrade creation. Admission
and the control-plane observer need to account for older agents so unsupported
requests do not sit Pending forever.

The agent in both A/B deployments needs to understand the saved journal and
recovery process. OS rollback and AgentUpgrade binary rollback are different
systems. Reusing code does not make their compatibility guarantees the same.

## 16. How we will verify it

The implementation plan needs tests for the cases below. Use disposable VMs
to check real boot and recovery behavior, not just mocked updater responses.

| Area | Cases to cover |
|---|---|
| API | Valid requests; missing backend; both/neither packages and releaseRef; invalid or empty package-name lists; unsupported channel; attempts to change inputs; rejected selectors and arbitrary parameters. |
| Routing | Agent handles HostUpgrade; provider/metalman ignore it; unsupported backend reports an error; old events cannot restart finished work. |
| Host model | Writable versus image-managed OS; executable present on an incompatible layout; missing service; unsupported updater version. |
| Saved inputs | Release/channel profile/configuration edited or deleted; artifact content changed behind the same name; Machine/operation recreated with a new UID. |
| Package update | All eligible packages, security-only channel, other supported OS-specific channel, named installed packages, names plus channel, unknown/uninstalled names, unsupported security metadata, dependency/channel rules, already current, native error, removals, package-lock contention, interrupted database recovery. |
| Saved state | Daemon stopped, updater service/client disconnected, missed completion event, API outage, reboot, damaged or incompatible journal. |
| Starting work | Crash before submission, after the updater accepts it, and before saving its ID or result; no blind duplicate install. |
| Activation | IfRequired/Always/Never, Never rejected for A/B or an uncertain reboot requirement, late discovery of a required reboot, restartNode with and without host reboot, no-op updates, updater-owned reboot, multiple reboots. |
| A/B boot | Expected target booted, wrong slot booted, fallback to old OS, boot hang, failed commit, competing health-confirmation services. |
| Rollback | Validate configured native guarantees; report unavailable rollback; report Failed after fallback; keep ownership accurate when rollback fails or its result is unknown. |
| Recovery | Backend-specific timing, configuration changes do not reset the deadline, no forced updater kill, fresh readiness versus an old Ready condition, credentials/state survive, old/new deployments can read the journal and shared data. |
| Overlapping work | Two controllers competing for a claim; an update started outside Unbounded; conflicts with power, replacement, reset, upgrade, and repave; an offline host keeps its claim despite expiry. |
| Reporting | Retry a failed status write without rerunning; reject stale writers; observer cannot assume native success. |
| Cleanup | Delete before start, delete during update, force deletion, orphaned work, TTL, log limits, stalled updater, manual recovery. |
| Security | Untrusted artifacts, disallowed configuration/hooks, hidden credentials in output, references to releases/configuration the caller cannot use. |

End-to-end tests must show that the host was updated, not just nspawn, and that
daemon restart and host reboot both resume the same update. A mock returning
success cannot prove A/B fallback or updater commit works.

## 17. Questions to settle before implementation planning

The design above is the proposed starting point. These choices still need
review. A planning agent should ask about them rather than quietly invent an
API or start a broad refactor.

### R1. Where do release definitions live?

Choose the release API, fields, scope, permissions, and storage for saved inputs.
Compare a dedicated release resource with versioned manifest references. Both
need typed inputs and a fixed identity for the update being run. Do not assume
a release CR already exists.

### R2. How do controllers share maintenance ownership?

Choose the claim representation, owner, fencing rules, and manual recovery path.
Every component that can run conflicting maintenance needs to respect the same
claim. Expiry alone cannot release it. Review the cross-package changes before
implementation.

### R3. Which backends and versions ship first?

Test each adapter separately and define its supported host setups. RAUC and
Trident are intended integrations, but that does not imply support for every
distribution variant, including Azure Container Linux. Ubuntu Core, Mender,
and OSTree remain future adapter candidates.

### R4. What exactly does each package upgrade do?

Define each adapter's supported channel names and their OS-specific meaning,
including how security-only updates are identified. Decide dependency behavior
across channel boundaries, literal installed-package matching, removals,
configuration files, native locks, restart detection, and interrupted transactions.

### R5. Who decides the new OS is healthy?

Define the fresh agent/Node evidence required and fit it to native boot deadlines.
Decide what happens during a control-plane outage and how to coordinate with
services that already commit deployments automatically. Define administrator-managed
recovery timing and rollback configuration for each backend, including how the
effective settings are saved for an operation.

### R6. Are the proposed fields and defaults right?

Review the simplified request and status names: backend, packages or releaseRef,
rebootPolicy, and restartNode. Check the IfRequired default, false restartNode
default, and how each adapter can reliably honor Never. Keep native rollback
settings and recovery timing out of the per-operation request.

### R7. Who observes an unavailable host and cleans up?

Choose the control-plane observer, how supported capabilities are reported,
the finalizer owner, and retention rules for orphaned work. Define how writers
avoid stale updates. Failed status does not prove execution stopped.

### R8. Do any deferred features belong in the first version?

Keep deferred activation, selectors, and automatic drain out unless their
ownership, rollout, and eviction behavior has been designed and reviewed.

### 17.1 Guidance for the agent preparing the implementation plan

1. Re-read the current code, test assertions, and related docs. This document
   records intent at one revision; the code may have changed.
2. Check which review questions have been settled. Ask about choices that would
   change the API or behavior before presenting a final plan.
3. Read the chosen RAUC/Trident versions' implementation and tests, then their
   docs. Verify what happens when starting an update, recovering, booting, and
   committing before choosing the adapter's API or connection method.
4. Include work for the API, agent, shared maintenance ownership, adapters, CLI,
   permissions, code generation, deployment, and testing.
5. Keep updater-specific code separate from the common workflow and Kubernetes
   status writes. Do not build a public arbitrary-command framework.
6. Keep shared-package changes focused and avoid imports across cmd packages.
   Ask before broad refactors or dependencies with broad impact.
7. Explain compatibility and deployment order, and what testing is needed before
   enabling each backend. Do not claim untested OS/backend combinations work.
8. Cover failure windows and real VM recovery, not just happy-path unit tests.
   Include the cases in section 16.
9. List follow-up edits to the lifecycle and host-operation docs, including the
   standalone SSH-host reboot discrepancy in section 3.2.

## 18. Alternatives considered

| Alternative | Why we chose a different approach |
|---|---|
| Arbitrary HostCommand | Gives callers more host privileges and makes requests harder to validate. |
| HostPackageUpgrade | Leaves image and A/B updates out of the name. |
| Use HostReplace for A/B | Treats an update that preserves identity like a rebuild of the host. |
| Backend only in MachineConfiguration | Hides the updater choice in separate configuration rather than making each request explicit. |
| Automatically detect or fall back between updaters | An installed tool may not be safe for this host. Falling back changes what the caller asked for. |
| Implement A/B inside Unbounded | Rebuilds functionality that belongs in the native updater and bootloader integration. |
| Run the update synchronously inside reconcile | Ties long-running work to the daemon and makes crashes around submission or completion hard to recover. |
| Declare success when the updater starts or reboot is accepted | Does not show that the update finished, the right OS booted, or the host recovered. |
