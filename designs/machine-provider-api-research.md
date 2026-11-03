# Machine Provider API Research Notes

Background for the [golden specification](golden-spec.md).
These comparisons informed the design; they are not additional requirements.
This is historical design research, not a current provider support matrix.
Lifecycle documentation was reviewed on 2026-09-23; maintenance and repair
documentation was reviewed on 2026-10-15. Verify current provider capabilities
before implementing an integration. The proposed
[maintenance reference](machine-provider-maintenance.md) defines the target
behavior separately from these native API comparisons.

## Cloud API mappings

Native equivalents for standalone VMs are listed below. Names follow each
provider's API terminology; integrations adapt their inputs and outcomes.
Availability depends on VM configuration and API version.

| Golden primitive | [Azure VM][azure-compute] | [GCP Compute Engine][gcp-instances] | [AWS EC2][aws-actions] | [OCI Compute][oci-compute] |
|---|---|---|---|---|
| Get / List | `Get`, `List` / `ListAll`; `InstanceView` for runtime state | `instances.get`, `list` / `aggregatedList` | `DescribeInstances` | `GetInstance`, `ListInstances` |
| Create | `CreateOrUpdate` | `instances.insert` | `RunInstances` | `LaunchInstance` |
| Delete | `Delete` | `instances.delete` | `TerminateInstances` | `TerminateInstance` |
| Reimage | [Reimage][azure-reimage] | [Boot-disk replacement workflow][gcp-boot] | [CreateReplaceRootVolumeTask][aws-root] | [UpdateInstance with boot-volume replacement][oci-boot] |
| Power on | `Start` | `instances.start` | `StartInstances` | `InstanceAction`: `START` |
| Power off | `PowerOff` (retains allocation) | `instances.stop` | `StopInstances` | `InstanceAction`: `STOP` / `SOFTSTOP` |
| Reboot | `Restart` | `instances.reset` | `RebootInstances` | `InstanceAction`: `RESET` / `SOFTRESET` |
| Startup payload | `osProfile.customData` | Metadata `startup-script` / `startup-script-url` | `UserData` | `metadata.user_data` |
| Track operation | [ARM polling URLs][azure-async] | [zoneOperations.get][gcp-operations] | Instance state; [root-volume task status][aws-root-status] | `GetInstance` lifecycle state |
| Retry token | Named-resource PUT; action-specific retry behavior | `requestId` on supported calls | `ClientToken` on supported calls | `opc-retry-token` on supported calls |
| Maintenance / repair | [Resource Health][azure-health]; [Scheduled Events][azure-events]; `performMaintenance` | `instances.get`: `resourceStatus.upcomingMaintenance`; `performMaintenance` | [DescribeInstanceStatus][aws-status] events; `ModifyInstanceEventStartTime` | `ListInstanceMaintenanceEvents` |
| Repair request | [Guest Health Reporting][azure-guest-health] (preview, HPC/GPU) | [reportHostAsFaulty][gcp-faulty] (A3/A4 GPU, reservation-bound) | `ReportInstanceStatus` (feedback only) | Not compared |
| Image catalog (optional) | `VirtualMachineImages.List` | `images.list` | `DescribeImages` | `ListImages` |

Key differences:

- **Reimage:** Azure's documented API supports image-version selection and
  reimage provisioning data for non-ephemeral OS disks; ephemeral disks reset
  to their initial state. GCP boot-disk replacement requires a stopped VM and
  multiple calls. AWS and OCI offer compatible OS-disk replacement, but fresh
  startup execution must also be arranged. OCI restricts replacement images
  to the same Linux distribution and does not allow updating launch
  `user_data`. An adapter may compose delete/create when needed.
- **Tracking:** AWS and OCI do not provide one common durable task resource
  for all these actions. The integration tracks requests and determines
  completion per action; observing a running VM alone cannot prove a requested
  reboot completed. Native retry guarantees and cancellation support must
  also be checked per action.
- **Maintenance:** Azure Scheduled Events obtained through guest metadata
  need a relay to reach an external controller. Azure also documents
  [Resource Graph delivery and control-plane acknowledgment][azure-events-graph]
  and [Event Grid delivery][azure-events-grid], both in preview at review.
  Coverage varies by provider and VM configuration; missing information stays
  unknown. AWS marks ended events by a `[Completed]` or `[Canceled]`
  description prefix; GCP and Azure guest metadata remove the event. These
  signals do not establish a verified healthy return to service. See
  [Maintenance and repair](#maintenance-and-repair) for a fuller comparison.

## Reimage and identity

Official documentation reviewed on 2026-09-23 establishes that both AWS and OCI
have ways to replace an OS disk without recreating the instance, subject to
constraints:

- [AWS EC2 root volume replacement](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/replace-root.html)
  can replace an EBS root volume using a compatible AMI on the existing
  instance. AWS documents retention of network interfaces, IP addresses,
  non-root volumes, and the physical host. The instance must be running and
  reboots during the operation. Image compatibility and instance-type
  restrictions apply; this is not a universal reimage facility.
- [OCI boot volume replacement](https://docs.oracle.com/en-us/iaas/Content/Compute/Tasks/replacingbootvolume.htm)
  explicitly operates without terminating and recreating the instance. It
  supports Linux images within the same distribution, excludes Windows and
  Marketplace images, and requires compatible launch options.
- [OCI instance update](https://docs.oracle.com/en-us/iaas/tools/oci-cli/latest/oci_cli_docs/cmdref/compute/instance/update.html)
  explicitly forbids changing, removing, or adding `user_data` and
  `ssh_authorized_keys` after launch. Replacing the boot volume does not by
  itself provide a way to replace launch bootstrap data through these fields.

The design implication is that OS disk replacement, fresh bootstrap delivery,
and preservation of identity/allocation must be considered separately. A
provider's supported path can depend on the requested image and bootstrap
requirements, not just its name.

## Infrastructure completion and bootstrap

Official documentation reviewed on 2026-09-23 separates infrastructure state
from arbitrary guest startup-script success in several comparable APIs:

| API | Documented behavior | Bootstrap implication |
|---|---|---|
| AWS EC2 | A root-volume replacement task reaches `succeeded` when the replacement volume is attached and the instance is available. User-data documentation separately instructs users to allow startup tasks to finish and inspect their output. | Task success is not a documented assertion that an agent-install script succeeded. |
| Azure with cloud-init | The guest can report ready before user custom-data processing finishes. Script errors are not fatal provisioning failures. | VM provisioning success does not establish bootstrap success. |
| Azure Linux Agent with custom-data execution enabled | Provisioning waits for script execution to finish, within its provisioning timeout, but script errors still are not fatal provisioning failures. | Waiting for execution and requiring successful execution are distinct guarantees. |
| Google Compute Engine | `RUNNING` includes a VM that is still booting. A guest agent separately executes metadata startup scripts. | The VM lifecycle state is insufficient evidence of bootstrap success. |
| OCI | Boot-volume replacement restores the prior instance state. Instance creation accepts cloud-init user data. The cited documentation does not promise that instance state reports arbitrary script success. | Do not infer bootstrap success from instance state without a separate execution result. |
| OpenStack Ironic | Deployment can supply cloud-init user data through a config drive; a first-boot service in the installed image applies it. Its documented deployment callbacks concern the deployment ramdisk. | These mechanisms do not establish a general user-data success acknowledgment; assuming one from deployment state alone would be an inference. |

Sources:

- [AWS root-volume task states](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/replace-root.html)
  and [AWS user-data execution](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/user-data.html).
- [Azure custom-data processing](https://learn.microsoft.com/en-us/azure/virtual-machines/custom-data#process-custom-data).
- [GCE instance lifecycle](https://docs.cloud.google.com/compute/docs/instances/instance-lifecycle)
  and [Linux startup scripts](https://docs.cloud.google.com/compute/docs/instances/startup-scripts/linux).
- [OCI boot-volume replacement](https://docs.oracle.com/en-us/iaas/Content/Compute/Tasks/replacingbootvolume.htm)
  and [instance creation](https://docs.oracle.com/en-us/iaas/Content/Compute/Tasks/launchinginstance.htm).
- [Ironic config-drive deployment](https://docs.openstack.org/ironic/latest/user/deploy.html#deploying-with-a-config-drive)
  and [deployment states](https://docs.openstack.org/ironic/latest/user/states.html).

A stronger completion guarantee can be built through explicit guest reporting.
For example, [AWS CloudFormation CreationPolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-creationpolicy.html)
can hold resource creation until it receives success signals, and fail on a
failure signal or timeout. This is an orchestration feature layered above the
base EC2 API.

## Idempotency conventions

Official documentation reviewed on 2026-09-23 provides the following examples.
The literal `Idempotency-Key` header is one convention; infrastructure APIs
also expose equivalent retry controls under other names.

| API | Mechanism | Documented scope or behavior |
|---|---|---|
| [Stripe](https://docs.stripe.com/api/idempotent_requests) | `Idempotency-Key` header | All POST requests accept keys. A retry returns the saved status code and body, including a saved server error. Keys may be pruned after at least 24 hours; changed parameters are rejected. |
| [Adyen](https://docs.adyen.com/development-resources/api-idempotency/) | `idempotency-key` header | Retried requests return the original response without duplicating processing. Keys are scoped to a company account and valid for 7 to 14 days; deduplication does not cross regional endpoints. |
| [AWS EC2](https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-idempotency.html) | `ClientToken` request parameter | Supported actions include `RunInstances`. Repeating a successful request with the same token and parameters causes no further action; incompatible parameter changes produce `IdempotentParameterMismatch`. Scope can be regional or zonal. |
| [OCI](https://docs.oracle.com/en-us/iaas/Content/API/Concepts/usingapi.htm) | `opc-retry-token` header | Supported operations can be retried without repeating the action. Tokens expire after 24 hours and can be invalidated earlier by conflicting operations. |
| [Google Compute Engine](https://docs.cloud.google.com/compute/docs/reference/rest/v1/instances/insert) | `requestId` query parameter | A nonzero UUID identifies retries so the server can ignore a duplicate request. Both instance creation and [instance reset](https://docs.cloud.google.com/compute/docs/reference/rest/v1/instances/reset) document this parameter. |

AWS also documents `ClientToken` on
[`CreateReplaceRootVolumeTask`](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_CreateReplaceRootVolumeTask.html),
and OCI explicitly documents a retry token on
[`launch_instance`](https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/client/oci.core.ComputeClient.html#oci.core.ComputeClient.launch_instance).

Support is operation-specific. These examples do not establish that every
provider supports deduplication for every lifecycle mutation, that a retry
always returns an identical response, or that all providers use `409` for a
key/payload conflict. Returning the same tracked operation and choosing
`409 Conflict` are proposed golden-contract semantics. A generic correlation
ID is not sufficient evidence that an API deduplicates requests.

## Maintenance and repair

Official documentation and API schemas reviewed on 2026-10-15:

| | AWS EC2 | GCP Compute Engine | Azure VM | CoreWeave |
|---|---|---|---|---|
| Control-plane signal | `DescribeInstanceStatus` events | `resourceStatus.upcomingMaintenance` | Scheduled Events via IMDS (guest only); control-plane view in preview | Kubernetes Node labels and conditions in CKS only |
| Stable ID; concurrent events | Yes | No; one event at a time | Yes | No |
| Explicit terminal state | No; `[Completed]` / `[Canceled]` description prefix | No; field is removed | `Completed` / `Canceled` only in preview Event Grid and Resource Graph delivery | No |
| Caller acknowledgment | None | None | `StartRequests` (guest); acknowledge (preview) | None; waits for idle workload |
| Trigger now / reschedule | Indirect via stop/start; `ModifyInstanceEventStartTime` | `performMaintenance`; reschedule on sole-tenant nodes only | `performMaintenance`; maintenance configurations | Reboot request via Node condition or `cwic`; no reschedule |
| Customer repair request | `ReportInstanceStatus`, feedback only | `reportHostAsFaulty` with typed reasons (A3/A4 GPU) | Guest Health Reporting (preview, HPC/GPU) | Cordon plus support ticket |
| Identity after repair | Same instance ID; Dedicated Host recovery assigns a new host ID | Same VM | Same VM; dedicated host redeploy (preview) changes asset ID | Triage removes the node; replacement is best effort |

The golden contract adopts the strongest element from each: stable IDs and
the reschedule deadline from AWS, a typed repair request from GCP,
`0`-means-no-impact duration and explicit terminal states from Azure's
control-plane view, and CoreWeave's separation of in-place repair from
replacement. It avoids the shared gaps: guest-only signals, completion
inferred from disappearance, silent event replacement, repair reports that
trigger nothing, and handoffs with no deadline.

Sources:

- [AWS scheduled events](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/monitoring-instances-status-check_sched.html),
  [ModifyInstanceEventStartTime](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ModifyInstanceEventStartTime.html),
  [ReportInstanceStatus](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ReportInstanceStatus.html),
  and [Dedicated Host recovery](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/dedicated-hosts-recovery.html).
- [GCP host maintenance monitoring](https://docs.cloud.google.com/compute/docs/instances/monitor-plan-host-maintenance-event),
  [triggering maintenance](https://docs.cloud.google.com/compute/docs/instances/trigger-host-maintenance-event),
  and [reporting a faulty host](https://docs.cloud.google.com/ai-hypercomputer/docs/manage/report-faulty-host).
- [Azure Scheduled Events](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/scheduled-events),
  [Resource Graph delivery](https://learn.microsoft.com/en-us/azure/virtual-machines/scheduled-events-resource-graph),
  [performMaintenance](https://learn.microsoft.com/en-us/rest/api/compute/virtual-machines/perform-maintenance),
  and [Guest Health Reporting](https://learn.microsoft.com/en-us/azure/azure-impact-reporting/guest-health-overview).
- [CoreWeave node state transitions](https://docs.coreweave.com/platform/fleet-management/node-lifecycle/state-transitions),
  [node reboots](https://docs.coreweave.com/products/cks/nodes/reboot),
  [cordon reasons](https://docs.coreweave.com/products/cks/nodes/cordon),
  and [node pools](https://docs.coreweave.com/products/cks/nodes/nodes-and-node-pools).

## Broader reference

The broader [provider operations reference](https://github.com/Azure/unbounded/blob/main/designs/machine-provider-operations-requirements.md)
covers additional capabilities. Its broader scope does not expand the pared-down
golden specification. Mapping this proposed target to an adapter is separate
implementation work.

[azure-compute]: https://learn.microsoft.com/en-us/rest/api/compute/virtual-machines?view=rest-compute-2025-04-01
[azure-reimage]: https://learn.microsoft.com/en-us/rest/api/compute/virtual-machines/reimage?view=rest-compute-2025-04-01
[azure-async]: https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/async-operations
[azure-health]: https://learn.microsoft.com/en-us/rest/api/resourcehealth/availability-statuses/get-by-resource?view=rest-resourcehealth-2025-05-01
[azure-events]: https://learn.microsoft.com/en-us/azure/virtual-machines/scheduled-events-overview
[azure-events-graph]: https://learn.microsoft.com/en-us/azure/virtual-machines/scheduled-events-resource-graph
[azure-events-grid]: https://learn.microsoft.com/en-us/azure/virtual-machines/scheduled-events-event-grid
[azure-guest-health]: https://learn.microsoft.com/en-us/azure/azure-impact-reporting/guest-health-overview
[gcp-instances]: https://docs.cloud.google.com/compute/docs/reference/rest/v1/instances
[gcp-boot]: https://docs.cloud.google.com/compute/docs/disks/detach-reattach-boot-disk
[gcp-operations]: https://docs.cloud.google.com/compute/docs/reference/rest/v1/zoneOperations/get
[gcp-faulty]: https://docs.cloud.google.com/ai-hypercomputer/docs/manage/report-faulty-host
[aws-actions]: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_Operations.html
[aws-root]: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/replace-root.html
[aws-root-status]: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeReplaceRootVolumeTasks.html
[aws-status]: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeInstanceStatus.html
[oci-compute]: https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/client/oci.core.ComputeClient.html
[oci-boot]: https://docs.oracle.com/en-us/iaas/Content/Compute/Tasks/replacingbootvolume.htm
