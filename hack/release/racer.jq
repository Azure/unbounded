# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Only the managed controller's leader-only readiness may be false. Ownership
# is checked separately against the live Deployment and its ReplicaSet UIDs.
def healthy_controller:
  . as $pod
  | .metadata.deletionTimestamp == null
    and .metadata.labels["racer.unbounded-cloud.io/component"] == "racer-controlplane"
    and .spec.serviceAccountName == "racer-controlplane"
    and (.spec.nodeName // "") != ""
    and .status.phase == "Running"
    and ((.spec.readinessGates // []) | length) == 0
    and all(["Initialized", "PodScheduled"][]; . as $condition |
      any($pod.status.conditions[]?; .type == $condition and .status == "True"))
    and ([.spec.containers[] | select(.name == "controller"
      and .readinessProbe.httpGet.path == "/readyz"
      and .readinessProbe.httpGet.port == "health"
      and .livenessProbe.httpGet.path == "/healthz"
      and .livenessProbe.httpGet.port == "health"
      and any(.ports[]?; .name == "health" and .containerPort == 8081))] | length) == 1
    and all(.spec.containers[]; .name as $name |
      any($pod.status.containerStatuses[]?; .name == $name
        and .state.running != null and .started == true
        and ($name == "controller" or .ready == true)))
    and all(.spec.initContainers[]?; . as $init |
      any($pod.status.initContainerStatuses[]?; .name == $init.name
        and (if $init.restartPolicy == "Always"
             then .state.running != null and .started == true and .ready == true
             else .state.terminated.exitCode == 0 end)));

# Include bootstrap and any release-owned sidecars. A third-party sidecar must
# not hide a stale controller or bootstrap image.
def release_images($registry; $tag):
  [.containers[], .initContainers[]?] | map(.image)
  | map(select(startswith($registry + "/"))) as $ours
  | ($ours | length) > 0 and all($ours[]; endswith(":" + $tag));
