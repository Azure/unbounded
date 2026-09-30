"""Render review-only JSON bundle from pinned bootstrap policy. Never applies.

Source CM must be precreated separately, then its actual UID pinned in policy.
Carrier image must contain static /racer-guard-launch plus /bin/cp; no build here.
"""

import argparse
import json
from pathlib import Path
import re

import contract as c


def bundle(policy, python_image, carrier_image):
    for image in (python_image, carrier_image):
        c.require(re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image), "digest-pinned image required")
    c.require(policy["sourceUID"] and len(policy["nodes"]) == 1500
              and set(policy["denyNodes"]) == c.DENY11, "complete pinned bootstrap policy required")
    ips = [c.ipv4(n["ip"]) for n in policy["nodes"].values()]
    c.require(len(set(ips)) == 1500 and len({n["uid"] for n in policy["nodes"].values()}) == 1500
              and all(n["uid"] for n in policy["nodes"].values()), "node pin collision")
    here = Path(__file__).parent
    code = {name: (here / name).read_text() for name in ("contract.py", "watcher.py", "local.py", "server.py", "bootstrap.py", "ready.py")}
    def obj(kind, name, **extra):
        return dict(apiVersion="apps/v1" if kind in ("Deployment", "DaemonSet") else "v1",
                    kind=kind, metadata=dict(name=name, namespace=c.NAMESPACE), **extra)
    program = obj("ConfigMap", c.PROGRAM_CM, immutable=True, data=code)
    fixed = obj("ConfigMap", c.POLICY_CM, immutable=True, data={"policy.json": c.canonical(policy)})
    env = [dict(name=name, valueFrom=dict(fieldRef=dict(fieldPath=field))) for name, field in
           (("NODE_NAME", "spec.nodeName"), ("NODE_IP", "status.hostIP"))]
    mounts = [dict(name="program", mountPath="/guard", readOnly=True),
              dict(name="policy", mountPath="/policy", readOnly=True)]
    volumes = [dict(name="program", configMap=dict(name=c.PROGRAM_CM)),
               dict(name="policy", configMap=dict(name=c.POLICY_CM))]
    security = dict(runAsUser=0, runAsGroup=0, allowPrivilegeEscalation=False, readOnlyRootFilesystem=True,
                    capabilities=dict(drop=["ALL"], add=["NET_ADMIN", "SYS_CHROOT", "NET_RAW"]),
                    seccompProfile=dict(type="RuntimeDefault"))
    guard_mounts = mounts + [dict(name="host", mountPath="/host", readOnly=True),
                             dict(name="state", mountPath="/run/racer-guard"),
                             dict(name="xtables", mountPath="/host/run/xtables.lock")]
    guard_volumes = volumes + [dict(name="host", hostPath=dict(path="/", type="Directory")),
                              dict(name="state", hostPath=dict(path="/run/racer-guard", type="DirectoryOrCreate")),
                              dict(name="xtables", hostPath=dict(path="/run/xtables.lock", type="File"))]
    base = dict(image=python_image, env=env, securityContext=security, volumeMounts=guard_mounts,
                resources=dict(requests=dict(cpu="10m", memory="64Mi"), limits=dict(cpu="250m", memory="128Mi")))
    guard = dict(base, name="guard", command=["python3", "-B", "/guard/server.py", "--policy", "/policy/policy.json"])
    guard["readinessProbe"] = dict(exec=dict(command=["timeout", "--signal=TERM", "--kill-after=10s", "20s",
        "python3", "-B", "/guard/ready.py", "--policy", "/policy/policy.json"]), timeoutSeconds=25, periodSeconds=10, failureThreshold=1)
    ds = obj("DaemonSet", "racer-stage47-dynamic-guard", spec=dict(
        selector=dict(matchLabels=dict(app="racer-stage47-dynamic-guard")),
        template=dict(metadata=dict(labels=dict(app="racer-stage47-dynamic-guard")), spec=dict(
            serviceAccountName="racer-guard-local", hostNetwork=True, dnsPolicy="ClusterFirstWithHostNet",
            tolerations=[dict(operator="Exists")], volumes=guard_volumes,
            affinity=dict(nodeAffinity=dict(requiredDuringSchedulingIgnoredDuringExecution=dict(nodeSelectorTerms=[dict(
                matchFields=[dict(key="metadata.name", operator="In", values=[name])]) for name in sorted(policy["nodes"])]))),
            containers=[guard]))))
    watcher = obj("Deployment", "racer-stage47-source-owner", spec=dict(replicas=2,
        selector=dict(matchLabels=dict(app="racer-stage47-source-owner")), template=dict(
            metadata=dict(labels=dict(app="racer-stage47-source-owner")), spec=dict(
                serviceAccountName="racer-guard-source-owner", volumes=volumes,
                containers=[dict(name="watcher", image=python_image, volumeMounts=mounts,
                    command=["python3", "-B",
                             "/guard/watcher.py", "--source-uid", policy["sourceUID"]],
                    securityContext=dict(runAsUser=65532, runAsGroup=65532, runAsNonRoot=True,
                        allowPrivilegeEscalation=False, readOnlyRootFilesystem=True,
                        capabilities=dict(drop=["ALL"]))) ]))))
    # Fragment only: apply through reviewed operator-supported configuration,
    # preserving all existing env/args/mounts and normal placement exclusions.
    dp_fragment = dict(volumes=[dict(name="guard-launch", emptyDir={}),
                               dict(name="guard-socket", hostPath=dict(path="/run/racer-guard", type="Directory"))],
        initContainers=[dict(name="guard-launch-copy", image=carrier_image,
                             command=["/bin/cp", "/racer-guard-launch", "/out/racer-guard-launch"],
                             volumeMounts=[dict(name="guard-launch", mountPath="/out")])],
        containers=[dict(name="dataplane", command=["/guard-launch/racer-guard-launch"],
                         env=[env[0]], volumeMounts=[dict(name="guard-launch", mountPath="/guard-launch", readOnly=True),
                             dict(name="guard-socket", mountPath="/run/racer-guard", readOnly=True)])])
    override = dict(component="racer", kind="DaemonSet", name=c.HOST_DS,
                    addInitContainers=["guard-launch-copy"], patch=dict(spec=dict(template=dict(spec=dp_fragment))))
    return dict(execution_authorized=False, objects=[program, fixed, watcher, ds], hostDPFragment=dp_fragment,
                hostDPOverride=override,
                prerequisites=["server tightens kubelet-created root-owned host directory to0700 on every guard start",
                    "allowLegacyTransition=true permits only exact-authority1511 old guard plus closed barrier insertion",
                    "review exact1500 matchFields node placement and existing guard coexistence",
                    "root DP image/config confirmed; preserve DENY11 normal placement and args",
                    "source CM precreated, RBAC separately reviewed, carrier binary/image externally supplied"])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy", required=True)
    parser.add_argument("--python-image", required=True)
    parser.add_argument("--carrier-image", required=True)
    args = parser.parse_args()
    print(json.dumps(bundle(json.loads(Path(args.policy).read_text()), args.python_image, args.carrier_image)))
