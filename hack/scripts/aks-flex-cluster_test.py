#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Offline tests for the self-contained AKS Flex deployment script.

Run: python3 hack/scripts/aks-flex-cluster_test.py
All temporary files stay inside the repository. No Azure calls or remote host
commands are executed; the fake tools persist a small Azure/Kubernetes model.
"""

import io
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack/scripts/aks-flex-cluster.sh"
SUBSCRIPTION = "11111111-1111-1111-1111-111111111111"

MOCK = r'''#!/usr/bin/env python3
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(os.environ["MOCK_ROOT"])
state_path = root / "cloud.json"
s = json.loads(state_path.read_text())
command = Path(sys.argv[0]).name
a = sys.argv[1:]
with (root / "calls.jsonl").open("a") as f:
    f.write(json.dumps([command, *a]) + "\n")
def save(): state_path.write_text(json.dumps(s))
def out(value):
    if "-o" in a and a[a.index("-o") + 1] == "none": return
    print(json.dumps(value) if not isinstance(value, str) else value)
def arg(*names, default=None):
    for n in names:
        if n in a: return a[a.index(n) + 1]
    return default
def tags():
    i = a.index("--tags") + 1
    values = []
    while i < len(a) and not a[i].startswith("-"):
        values.append(a[i]); i += 1
    return dict(v.split("=", 1) for v in values)
def resource_id(t, name):
    return f"/subscriptions/{os.environ['MOCK_SUBSCRIPTION']}/resourceGroups/flexdemo/providers/{t}/{name}"
def add(t, name, **properties):
    r = dict(id=resource_id(t, name), name=name, type=t, location="eastus2", tags=tags(), **properties)
    s["resources"][t + "/" + name] = r
    save()
    return r
def get(t, name): return s["resources"][t + "/" + name]

group = arg("-g", "--resource-group")
name = arg("--name")
if command == "az":
    if a[:1] == ["version"]: out(s.get("cli_version","2.90.0"))
    elif a[:2] == ["account", "show"]: out(os.environ["MOCK_SUBSCRIPTION"])
    elif a[:2] == ["extension", "list"]: out([{"name":"aks-preview","version":"22.0.0b8"}] if s.get("extension_installed",True) else [])
    elif a[:2] == ["extension", "add"]: s["extension_installed"] = True; save()
    elif a[:2] == ["feature", "show"]: out(s.get("features",{}).get(name,"Registered"))
    elif a[:2] == ["feature", "register"]: s.setdefault("features",{})[name] = "Registered"; save()
    elif a[:2] == ["provider", "register"]: pass
    elif a[:3] == ["role", "definition", "list"]:
        out([] if s.get("missing_role") else [{"name":name,"roleType":"BuiltInRole"}])
    elif a[:2] == ["group", "list"]: out(list(s["groups"].values()))
    elif a[:2] == ["group", "create"]:
        s["groups"][name] = {"name":name,"location":"eastus2","tags":tags()}; save()
    elif a[:2] == ["resource", "list"]:
        t = arg("--resource-type")
        if s.get("fail_list") == t:
            print("AuthorizationFailed", file=sys.stderr); sys.exit(1)
        out([v for v in s["resources"].values() if v["type"] == t])
    elif a[:3] == ["network", "vnet", "create"]:
        add("Microsoft.Network/virtualNetworks", name, addressSpace={"addressPrefixes":[arg("--address-prefixes")]}, subnets=[])
    elif a[:3] == ["network", "vnet", "show"]: out(get("Microsoft.Network/virtualNetworks", name))
    elif a[:4] == ["network", "vnet", "subnet", "list"]:
        out(get("Microsoft.Network/virtualNetworks", arg("--vnet-name"))["subnets"])
    elif a[:4] == ["network", "vnet", "subnet", "create"]:
        v = get("Microsoft.Network/virtualNetworks", arg("--vnet-name"))
        subnet = {"name":name,"addressPrefix":arg("--address-prefixes"),"id":v["id"]+"/subnets/"+name}
        nsg = arg("--network-security-group")
        if nsg: subnet["networkSecurityGroup"] = {"id":nsg}
        v["subnets"].append(subnet); save()
    elif a[:3] == ["network", "nsg", "create"]:
        add("Microsoft.Network/networkSecurityGroups", name, securityRules=[])
    elif a[:3] == ["network", "nsg", "show"]: out(get("Microsoft.Network/networkSecurityGroups", name))
    elif a[:4] == ["network", "nsg", "rule", "create"]:
        nsg = get("Microsoft.Network/networkSecurityGroups", arg("--nsg-name"))
        nsg["securityRules"] = [{"name":name,"sourceAddressPrefix":arg("--source-address-prefixes"),
            "destinationPortRange":"22","sourcePortRange":"*","destinationAddressPrefix":"*",
            "priority":100,"direction":"Inbound","protocol":"Tcp","access":"Allow"}]; save()
    elif a[:2] == ["identity", "create"]:
        add("Microsoft.ManagedIdentity/userAssignedIdentities", name, principalId="controlplane-principal")
    elif a[:2] == ["identity", "show"]: out(get("Microsoft.ManagedIdentity/userAssignedIdentities", name))
    elif a[:3] == ["role", "assignment", "list"]:
        out([r for r in s["roles"] if r["scope"] == arg("--scope")])
    elif a[:3] == ["role", "assignment", "create"]:
        s["roles"].append({"principalId":arg("--assignee-object-id"),"roleDefinitionId":"/roles/"+arg("--role"),"scope":arg("--scope")}); save()
    elif a[:2] == ["aks", "create"]:
        add("Microsoft.ContainerService/managedClusters", name, provisioningState="Succeeded",kubernetesVersion="1.36.2",currentKubernetesVersion="1.36.2",
            networkProfile={"networkPlugin":"none","serviceCidr":arg("--service-cidr"),"dnsServiceIp":arg("--dns-service-ip"),"podCidr":arg("--pod-cidr")},
            agentPoolProfiles=[{"name":"system","mode":"System","count":1,"vmSize":arg("--node-vm-size"),"vnetSubnetId":arg("--vnet-subnet-id")}])
    elif a[:2] == ["aks", "show"]: out(get("Microsoft.ContainerService/managedClusters", name))
    elif a[:2] == ["aks", "get-credentials"]: Path(arg("--file")).write_text("fake kubeconfig\n")
    elif a[:3] == ["aks", "operation", "show-latest"]: out({"status":"Succeeded"})
    elif a[:3] == ["aks", "nodepool", "list"]:
        out([] if not s.get("pool") else [{"name":"flexpool"}])
    elif a[:1] == ["rest"]:
        method, url = arg("--method"), arg("--url")
        if method == "put":
            body = json.loads(Path(arg("--body")[1:]).read_text())
            assert body["properties"]["type"] == "FlexNodes"
            assert "count" not in body["properties"]
            s["pool"] = {"properties":{**body["properties"],"provisioningState":"Succeeded"}}; save()
        elif "/machines?" in url:
            out({"value":[{"name":n,"properties":{"provisioningState":"Succeeded"}} for n in s["joined"]]})
        else: out(s["pool"])
    elif a[:3] == ["network", "public-ip", "create"]:
        number = name.split("-flex")[1].split("-")[0]
        add("Microsoft.Network/publicIPAddresses", name, sku={"name":"Standard"},publicIPAllocationMethod="Static",ipAddress="20.30.40."+number)
    elif a[:3] == ["network", "public-ip", "show"]: out(get("Microsoft.Network/publicIPAddresses", name))
    elif a[:3] == ["network", "nic", "create"]:
        number = name.split("-flex")[1].split("-")[0]
        add("Microsoft.Network/networkInterfaces", name,enableIPForwarding=True,
            ipConfigurations=[{"subnet":{"id":arg("--subnet")},"publicIPAddress":{"id":arg("--public-ip-address")},"privateIPAddress":"10.91.1."+str(3+int(number))}])
    elif a[:3] == ["network", "nic", "show"]: out(get("Microsoft.Network/networkInterfaces", name))
    elif a[:2] == ["vm", "create"]:
        add("Microsoft.Compute/virtualMachines", name,hardwareProfile={"vmSize":arg("--size")},
            osProfile={"computerName":name},storageProfile={"imageReference":{"publisher":"Canonical","offer":"ubuntu-24_04-lts"}},
            networkProfile={"networkInterfaces":[{"id":arg("--nics")}]},identity={"type":"SystemAssigned","principalId":name+"-principal"})
    elif a[:2] == ["vm", "show"]: out(get("Microsoft.Compute/virtualMachines", name))
    elif a[:3] == ["vm", "run-command", "invoke"]:
        if s.get("run_command_api_failure"):
            print("VM Run Command extension unavailable", file=sys.stderr); sys.exit(1)
        wrapper = Path(arg("--scripts")[1:]).read_text()
        marker = next(line.split("=",1)[1] for line in wrapper.splitlines() if line.startswith("marker="))
        encoded = wrapper.split("<<'DEPLOYMENT_BASE64'\n",1)[1].split("\nDEPLOYMENT_BASE64",1)[0]
        (root / "remote.sh").write_bytes(base64.b64decode(encoded))
        if s.get("execute_run_command"):
            # Execute only a sandboxed wrapper supplied by the unit test, never
            # a real host bootstrap. All /run writes stay in this fixture.
            sandboxed = root / "sandboxed-run-command.sh"
            sandboxed.write_text(wrapper.replace("/run/aks-flex-node-deployment.sh",str(root / "payload.sh")))
            result = subprocess.run(["/bin/sh",str(sandboxed)],env=os.environ,text=True,capture_output=True,timeout=10)
            message = "Enable succeeded:\n[stdout]\n"+result.stdout+"\n[stderr]\n"+result.stderr
            s["executed_run_command_shell"] = "/bin/sh"; save()
        else:
            code = s.get("run_command_exit",0)
            if s.get("fail_join_once") == name:
                s.pop("fail_join_once"); code = 78; save()
            status = "already-joined" if name in s["joined"] else "joined"
            if code == 0 and name not in s["joined"]:
                s["joined"].append(name); s["bootstraps"][name] = s["bootstraps"].get(name,0)+1; save()
            message = f"FLEX_STATUS={status} node={name}\n{marker}={code}\n"
            if s.get("missing_run_command_marker"): message = "remote script did not finish"
            if s.get("run_command_marker_mode") == "duplicate": message += f"{marker}={code}\n"
            if s.get("run_command_marker_mode") == "stale": message = message.replace(marker,"__AKS_FLEX_REMOTE_EXIT_old__")
            if s.get("run_command_crlf"): message = message.replace("\n","\r\n")
            message = "Enable succeeded:\n[stdout]\n"+message+"\n[stderr]\n"
        out({"value":[{"code":"ProvisioningState/succeeded","message":message}]})
    elif a[:2] == ["vm", "get-instance-view"]: out(s.get("power",{}).get(name,"PowerState/running"))
    elif a[:2] == ["vm", "start"]:
        s.setdefault("power",{})[name] = "PowerState/running"; save()
    else: raise RuntimeError((command,a))
elif command == "curl":
    file = arg("-o")
    url = next(x for x in a if x.startswith("https://"))
    if url == "https://api.ipify.org": out("8.8.4.4")
    elif url.endswith("checksums.txt"):
        if "AKSFlexNode" in url:
            Path(file).write_text("a"*64+"  aks-flex-node-linux-amd64.tar.gz\n")
        else:
            archive = root / "plugin.tar.gz"
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            Path(file).write_text("".join(digest+"  kubectl-unbounded-"+os+"-"+arch+".tar.gz\n"
                for os in ["linux","darwin"] for arch in ["amd64","arm64"]))
    elif url.endswith("tar.gz"): Path(file).write_bytes((root / "plugin.tar.gz").read_bytes())
    elif url.endswith("bootstrap.sh"): Path(file).write_text("#!/bin/bash\nexit 0\n")
    else: raise RuntimeError(url)
elif command in ["kubectl", "kubectl-unbounded"]:
    if command == "kubectl-unbounded": s["operator"] = True; save()
    elif "apply" in a:
        manifest = json.loads(Path(arg("-f")).read_text())
        s.setdefault("manifests",[]).append(manifest)
        for item in manifest.get("items",[]):
            if item["kind"] == "Namespace":
                s["namespaces"] = [item]
        save()
    elif "exec" in a:
        if s.get("fail_smoke_exec"):
            print("smoke workload connection failed",file=sys.stderr); sys.exit(1)
    elif "rollout" in a or "wait" in a or "logs" in a: pass
    elif "get" in a:
        kind = a[a.index("get")+1]
        if s.get("fail_get") == kind:
            print("Kubernetes API forbidden",file=sys.stderr); sys.exit(1)
        if kind == "namespaces": out({"items":s.get("namespaces",[])})
        elif kind == "deployments": out({"items":[] if not s.get("operator") else [{"metadata":{"name":"unbounded-operator","namespace":"unbounded-system"}}]})
        elif kind == "pods":
            out({"items":[{"metadata":{"name":n+"-echo"},"status":{"podIP":"10.95."+str(i)+".2"}} for i,n in enumerate(s["joined"]) ]})
        elif kind.startswith("node/"):
            out({"metadata":{"labels":s.get("node_labels",{"kubernetes.azure.com/agentpool":"flexpool","unbounded-cloud.io/site":"flex-site"})}})
        elif kind == "nodes": pass
        elif kind.startswith(("deployment/", "daemonset/")):
            remaining = s.get("get_missing",{}).get(kind,0)
            if remaining:
                s["get_missing"][kind] = remaining - 1; save()
            else: out(kind)
        else: raise RuntimeError((command,a))
    else: raise RuntimeError((command,a))
elif command == "ssh":
    if a[-1] == "true": sys.exit(1 if s.get("fail_ssh_connection") else 0)
    body = sys.stdin.read()
    (root / "remote.sh").write_text(body)
    number = next(x for x in a if x.startswith("azureuser@")).rsplit(".",1)[1]
    node = "flexdemo-flex"+number
    if s.get("fail_join_once") == node:
        s.pop("fail_join_once"); save(); sys.exit(78)
    if node not in s["joined"]:
        s["joined"].append(node); s["bootstraps"][node] = s["bootstraps"].get(node,0)+1; save()
        out("FLEX_STATUS=joined node="+node)
    else: out("FLEX_STATUS=already-joined node="+node)
elif command == "sleep": pass
elif command == "timeout":
    assert a[:2] == ["--signal=TERM","--kill-after=30s"]
    assert a[3] == "bash" and Path(a[4]).resolve().parent == root.resolve()
    os.execvp(a[3],a[3:])
elif command == "systemctl":
    if a[0] == "start": s["host_active"] = True; save()
    elif a[0] == "is-active": sys.exit(0 if s.get("host_active",True) else 1)
    else: raise RuntimeError((command,a))
else: raise RuntimeError(command)
'''


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        (ROOT / "tmp").mkdir(exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="aks-flex-test-", dir=ROOT / "tmp")
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.model = self.root / "cloud.json"
        self.model.write_text(json.dumps({"groups": {}, "resources": {}, "roles": [], "joined": [], "bootstraps": {}}))
        for tool in ("az", "curl", "kubectl", "kubectl-unbounded", "ssh", "systemctl", "sleep", "timeout"):
            file = self.bin / tool
            file.write_text(MOCK)
            file.chmod(0o700)
        with tarfile.open(self.root / "plugin.tar.gz", "w:gz") as archive:
            info = tarfile.TarInfo("kubectl-unbounded")
            content = MOCK.encode()
            info.size = len(content)
            info.mode = 0o700
            archive.addfile(info, io.BytesIO(content))
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}", MOCK_ROOT=str(self.root), MOCK_SUBSCRIPTION=SUBSCRIPTION)
        self.args = ["bash", str(SCRIPT), "--name", "flexdemo", "--location", "eastus2", "--subscription", SUBSCRIPTION,
                     "--state-dir", str(self.root / "state"), "--ssh-source-ip", "8.8.4.4", "--yes"]

    def tearDown(self):
        self.temp.cleanup()

    def run_script(self, extra=(), success=True):
        result = subprocess.run([*self.args, *extra], env=self.env, text=True, capture_output=True, timeout=30)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def state(self):
        return json.loads(self.model.read_text())

    def modify(self, **changes):
        state = self.state()
        state.update(changes)
        self.model.write_text(json.dumps(state))

    def calls(self):
        file = self.root / "calls.jsonl"
        return [json.loads(line) for line in file.read_text().splitlines()] if file.exists() else []

    def assert_no_acr_wiring(self):
        state = self.state()
        for call in self.calls():
            if call[0] == "az":
                self.assertNotEqual(call[1], "acr")
                self.assertNotEqual(call[1:3], ["aks", "update"])
                self.assertNotEqual(call[1:3], ["identity", "federated-credential"])
                if call[1] == "identity":
                    self.assertEqual(call[call.index("--name") + 1], "flexdemo-controlplane")
            for forbidden in ("gantry-config", "node-auth.yaml", "registry-auth-node",
                              "gantry-acr-auth", "--attach-acr", "--enable-oidc-issuer",
                              "--enable-workload-identity", "--enable-identity-binding"):
                self.assertNotIn(forbidden, call)
        allowed_roles = {"4d97b98b-1d4f-4787-a291-c67834d212e7", "8f139b0f-7eaf-460b-a9da-5b1246d9ed0d"}
        self.assertTrue(all(role["roleDefinitionId"].split("/")[-1] in allowed_roles for role in state["roles"]))
        for manifest in state["manifests"]:
            payload = json.dumps(manifest)
            for forbidden in ("azurecr.io", "node-auth.yaml", "imagePullSecrets", "serviceAccountName"):
                self.assertNotIn(forbidden, payload)
        remote = (self.root / "remote.sh").read_text()
        for forbidden in ("azurecr.io", "acr-credential-provider", "registry-auth", "node-auth.yaml"):
            self.assertNotIn(forbidden, remote)

    def test_complete_run_and_no_duplicate_resources_on_rerun(self):
        self.run_script()
        first = self.state()
        calls = len(self.calls())
        result = self.run_script()
        second = self.state()
        self.assertEqual(len(first["resources"]), 13)  # VNet, NSG, identity, AKS, 3x(PIP, NIC, VM)
        self.assertEqual(first["resources"], second["resources"])
        self.assertEqual(len(second["roles"]), 4)
        cluster = second["resources"]["Microsoft.ContainerService/managedClusters/flexdemo"]
        flex_roles = [role for role in second["roles"] if role["roleDefinitionId"].endswith("/8f139b0f-7eaf-460b-a9da-5b1246d9ed0d")]
        self.assertEqual(len(flex_roles), 3)
        self.assertEqual({role["scope"] for role in flex_roles}, {cluster["id"] + "/agentPools/flexpool"})
        self.assertEqual({role["principalId"] for role in flex_roles}, {f"flexdemo-flex{i}-principal" for i in range(1, 4)})
        network_role = next(role for role in second["roles"] if role["principalId"] == "controlplane-principal")
        self.assertTrue(network_role["roleDefinitionId"].endswith("/4d97b98b-1d4f-4787-a291-c67834d212e7"))
        self.assertEqual(network_role["scope"], second["resources"]["Microsoft.Network/virtualNetworks/flexdemo-vnet"]["id"])
        rules = second["resources"]["Microsoft.Network/networkSecurityGroups/flexdemo-flex-nsg"]["securityRules"]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["sourceAddressPrefix"], "8.8.4.4/32")
        self.assertEqual(rules[0]["destinationPortRange"], "22")
        self.assertEqual(second["bootstraps"], {f"flexdemo-flex{i}": 1 for i in range(1, 4)})
        self.assertEqual(result.stdout.count("already-joined"), 3)
        self.assertIn("Bootstrap transport:      run-command", result.stdout)
        self.assertFalse(any(call[0] == "ssh" for call in self.calls()))
        rerun = self.calls()[calls:]
        self.assertFalse(any(call[0] == "az" and "create" in call for call in rerun))
        self.assertFalse(any(call[0] == "az" and "put" in call for call in rerun))
        self.assertFalse(any(call[0] == "az" and "set" in call for call in self.calls()))
        self.assertTrue(all("--subscription" in call for call in self.calls() if call[0] == "az" and call[1] not in ("version", "extension")))
        state_dir = self.root / "state"
        self.assertEqual((state_dir / "kubeconfig").stat().st_mode & 0o777, 0o600)
        self.assertEqual((state_dir / "id_ed25519").stat().st_mode & 0o777, 0o600)
        self.assertFalse((state_dir / ".lock").exists())
        access = (state_dir / "ssh-access.txt").read_text()
        self.assertEqual(access.count("ssh -i"), 3)
        sites = json.loads((state_dir / "sites.json").read_text())["items"]
        self.assertEqual(sites[2]["spec"], {"sites": ["cluster", "flex-site"], "meshNodes": True, "tunnelProtocol": "Auto"})
        self.assertFalse(any(item["kind"] in ("GatewayPool", "SiteGatewayPoolAssignment") for item in sites))
        remote = (self.root / "remote.sh").read_text()
        self.assertIn("--agent-sha256", remote)
        self.assertIn("existing agent belongs to another cluster", remote)
        self.assertIn("partial node deployment exists", remote)
        self.assertIn("NODE=flexdemo-flex3", remote)
        self.assertNotIn("aks-flex-node reset", remote)
        subprocess.run(["bash", "-n", str(self.root / "remote.sh")], check=True)
        self.assertEqual(sum(call[0] == "kubectl" and "exec" in call for call in self.calls()), 6)
        self.assertEqual(sum(call[0] == "kubectl-unbounded" for call in self.calls()), 1)
        smoke = json.loads((state_dir / "smoke.json").read_text())["items"][1]
        self.assertEqual(smoke["spec"]["template"]["spec"]["nodeSelector"], {"kubernetes.azure.com/agentpool": "flexpool"})
        self.assert_no_acr_wiring()
        self.assertEqual(list(state_dir.glob("registry-*")), [])

    def test_waits_for_asynchronous_operator_workload_creation(self):
        self.modify(get_missing={"deployment/unbounded-net-controller":2,"daemonset/unbounded-net-node":1,
                                 "daemonset/unbounded-net-kube-proxy-flex-site":1})
        self.run_script()
        self.assertTrue(all(count == 0 for count in self.state()["get_missing"].values()))
        gets = [call for call in self.calls() if call[0] == "kubectl" and "deployment/unbounded-net-controller" in call and "get" in call]
        self.assertEqual(len(gets), 3)

    def test_run_command_transport_reuses_hosts_without_ssh(self):
        self.run_script(["--bootstrap-transport", "run-command"])
        self.run_script(["--bootstrap-transport", "run-command"])
        self.assertEqual(self.state()["bootstraps"], {f"flexdemo-flex{i}": 1 for i in range(1,4)})
        self.assertFalse(any(call[0] == "ssh" for call in self.calls()))
        self.assertEqual(sum(call[:4] == ["az","vm","run-command","invoke"] for call in self.calls()), 6)
        for file in (self.root / "state").glob("run-command-*.sh"):
            subprocess.run(["bash", "-n", str(file)], check=True)

    def test_run_command_shell_failure_is_not_extension_success(self):
        self.modify(run_command_exit=78)
        result = self.run_script(["--bootstrap-transport", "run-command"], success=False)
        self.assertIn("with exit 78", result.stderr)
        self.assertEqual(self.state()["joined"], [])

    def test_run_command_requires_remote_completion_marker(self):
        self.modify(missing_run_command_marker=True)
        result = self.run_script(["--bootstrap-transport", "run-command"], success=False)
        self.assertIn("did not report a shell exit code", result.stderr)

    def test_run_command_accepts_crlf_completion_marker(self):
        self.modify(run_command_crlf=True)
        self.run_script(["--flex-count", "1"])
        self.assertEqual(self.state()["joined"], ["flexdemo-flex1"])

    def test_run_command_rejects_duplicate_or_stale_completion_marker(self):
        for mode in ("duplicate", "stale"):
            with self.subTest(mode=mode):
                self.modify(run_command_marker_mode=mode)
                result = self.run_script(["--flex-count", "1"], success=False)
                self.assertIn("did not report a shell exit code", result.stderr)
                self.assertNotIn("Deployment ready:", result.stderr)

    def test_run_command_api_failure_is_not_bootstrap_success(self):
        self.modify(run_command_api_failure=True)
        result = self.run_script(["--flex-count", "1"], success=False)
        self.assertIn("Azure Run Command failed", result.stderr)
        self.assertEqual(self.state()["joined"], [])

    def execute_run_command_wrapper(self, payload, success):
        state = self.root / "state"
        state.mkdir()
        file = self.root / "unit-payload.sh"
        file.write_text(payload)
        self.modify(execute_run_command=True)
        result = subprocess.run(
            ["bash", "-c", 'source "$1"; STATE_DIR="$2"; SUBSCRIPTION="$3"; RESOURCE_GROUP=flexdemo; SSH_SOURCE_IP=8.8.4.4; run_vm_command flexdemo-flex1 "$4"',
             "test", str(SCRIPT), str(state), SUBSCRIPTION, str(file)],
            env=self.env, text=True, capture_output=True, timeout=15,
        )
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertEqual(self.state()["executed_run_command_shell"], "/bin/sh")
        wrapper = state / "run-command-flexdemo-flex1.sh"
        subprocess.run(["/bin/sh", "-n", str(wrapper)], check=True)
        subprocess.run(["bash", "-n", str(wrapper)], check=True)
        return result

    def test_run_command_launcher_runs_bash_payload_under_posix_handler(self):
        result = self.execute_run_command_wrapper(
            '#!/bin/bash\nset -euo pipefail\nvalues=(alpha beta)\n[[ "${values[1]}" == beta ]]\nprintf "sandbox payload ran\\n"\n',
            success=True,
        )
        self.assertIn("sandbox payload ran", result.stdout)
        self.assertFalse((self.root / "payload.sh").exists())

    def test_run_command_launcher_preserves_actual_payload_failure(self):
        result = self.execute_run_command_wrapper("#!/bin/bash\nexit 78\n", success=False)
        self.assertIn("with exit 78", result.stderr)

    def test_ssh_transport_remains_available_and_idempotent(self):
        options = ["--bootstrap-transport", "ssh", "--flex-count", "1"]
        self.run_script(options)
        result = self.run_script(options)
        self.assertEqual(self.state()["bootstraps"], {"flexdemo-flex1": 1})
        self.assertIn("already-joined", result.stdout)
        self.assertTrue(any(call[0] == "ssh" for call in self.calls()))
        self.assertFalse(any(call[:4] == ["az", "vm", "run-command", "invoke"] for call in self.calls()))

    def test_ssh_failure_does_not_automatically_change_transport(self):
        self.modify(fail_ssh_connection=True)
        result = self.run_script(["--bootstrap-transport", "ssh", "--flex-count", "1", "--timeout", "1"], success=False)
        self.assertIn("SSH unavailable", result.stderr)
        self.assertEqual(self.state()["joined"], [])
        self.assertFalse(any(call[:4] == ["az", "vm", "run-command", "invoke"] for call in self.calls()))

    def test_partial_run_resumes_without_rebootstrapping_first_node(self):
        self.modify(fail_join_once="flexdemo-flex2")
        self.run_script(success=False)
        self.assertEqual(self.state()["joined"], ["flexdemo-flex1"])
        self.run_script()
        self.assertEqual(self.state()["bootstraps"], {f"flexdemo-flex{i}": 1 for i in range(1, 4)})

    def test_public_ip_change_updates_only_ssh_rule(self):
        self.run_script()
        first = self.state()
        call_count = len(self.calls())
        self.run_script(["--ssh-source-ip", "1.1.1.1"])
        second = self.state()
        rule = second["resources"]["Microsoft.Network/networkSecurityGroups/flexdemo-flex-nsg"]["securityRules"][0]
        self.assertEqual(rule["sourceAddressPrefix"], "1.1.1.1/32")
        self.assertEqual(first["bootstraps"], second["bootstraps"])
        creates = [call for call in self.calls()[call_count:] if call[0] == "az" and "create" in call]
        self.assertEqual(len(creates), 1)
        self.assertEqual(creates[0][1:5], ["network", "nsg", "rule", "create"])

    def test_configuration_changes_are_rejected_before_cloud_mutations(self):
        self.run_script()
        before = len(self.calls())
        result = self.run_script(["--vm-size", "Standard_D8s_v6"], success=False)
        self.assertIn("provisioning arguments differ", result.stderr)
        self.assertFalse(any("create" in call for call in self.calls()[before:]))

    def test_unowned_resource_group_is_not_adopted(self):
        self.modify(groups={"flexdemo": {"name": "flexdemo", "location": "eastus2", "tags": {}}})
        result = self.run_script(success=False)
        self.assertIn("refusing to modify", result.stderr)
        self.assertEqual(self.state()["resources"], {})

    def test_failed_resource_query_is_not_treated_as_absence(self):
        self.modify(fail_list="Microsoft.Network/virtualNetworks")
        result = self.run_script(success=False)
        self.assertIn("not treating a failed query as a missing resource", result.stderr)
        self.assertEqual(self.state()["resources"], {})

    def test_preview_extension_and_features_are_installed_only_once(self):
        self.modify(extension_installed=False, features={"AKSFlexNodePreview": "NotRegistered", "PutMachinePreview": "NotRegistered"})
        self.run_script()
        self.run_script()
        calls = self.calls()
        self.assertEqual(sum(call[:3] == ["az", "extension", "add"] for call in calls), 1)
        self.assertEqual(sum(call[:3] == ["az", "feature", "register"] for call in calls), 2)
        self.assertTrue(all(state == "Registered" for state in self.state()["features"].values()))

    def test_old_azure_cli_stops_before_provisioning(self):
        self.modify(cli_version="2.89.0")
        result = self.run_script(success=False)
        self.assertIn("Azure CLI >= 2.90.0 required", result.stderr)
        self.assertEqual(self.state()["resources"], {})
        self.assertFalse((self.root / "state").exists())

    def test_missing_preview_role_has_no_admin_fallback(self):
        self.modify(missing_role=True)
        result = self.run_script(success=False)
        self.assertIn("no Contributor/admin fallback", result.stderr)
        self.assertEqual(self.state()["groups"], {})
        self.assertEqual(self.state()["roles"], [])

    def test_stopped_vm_is_started_without_recreating_it(self):
        self.run_script()
        self.modify(power={"flexdemo-flex2": "PowerState/deallocated"})
        before = len(self.calls())
        self.run_script()
        starts = [call for call in self.calls()[before:] if call[:3] == ["az", "vm", "start"]]
        self.assertEqual(len(starts), 1)
        self.assertEqual(self.state()["power"]["flexdemo-flex2"], "PowerState/running")
        self.assertEqual(self.state()["bootstraps"]["flexdemo-flex2"], 1)

    def test_single_flex_node_option(self):
        self.run_script(["--flex-count", "1"])
        self.assertEqual(self.state()["joined"], ["flexdemo-flex1"])
        self.assertEqual(len(self.state()["resources"]), 7)

    def remote_script(self, config=None, partial=False):
        state = self.root / "state"
        cache = state / "cache"
        cache.mkdir(parents=True)
        (cache / "bootstrap-v0.2.0.sh").write_text("#!/bin/bash\nexit 0\n")
        remote = self.root / "remote-guard.sh"
        cluster_id = f"/subscriptions/{SUBSCRIPTION}/resourceGroups/flexdemo/providers/Microsoft.ContainerService/managedClusters/flexdemo"
        subprocess.run(["bash", "-c", 'source "$1"; STATE_DIR="$2"; AKS_ID="$3"; VM_PRIVATE_IP=10.91.1.4; AGENT_DIGEST=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef; write_remote_script flexdemo-flex1 "$4"',
                        "test", str(SCRIPT), str(state), cluster_id, str(remote)], check=True, env=self.env)
        host = self.root / "host"
        body = remote.read_text().replace("/etc/aks-flex-node", str(host / "etc/aks-flex-node"))
        body = body.replace("/etc/systemd", str(host / "etc/systemd"))
        remote.write_text(body)
        if config is not None:
            unit = host / "etc/systemd/system/aks-flex-node-agent.service"
            unit.parent.mkdir(parents=True)
            unit.touch()
            file = host / "etc/aks-flex-node/config.json"
            file.parent.mkdir(parents=True)
            file.write_text(json.dumps({"azure":{"targetCluster":{"resourceId":config},"targetAgentPoolName":"flexpool"},
                                        "agent":{"nodeName":"flexdemo-flex1"},"node":{"kubelet":{"nodeIP":"10.91.1.4"}}}))
        if partial:
            unit = host / "etc/systemd/nspawn/kube1.nspawn"
            unit.parent.mkdir(parents=True)
            unit.touch()
        return remote, cluster_id

    def test_remote_already_joined_guard_executes_without_bootstrap(self):
        cluster_id = f"/subscriptions/{SUBSCRIPTION}/resourceGroups/flexdemo/providers/Microsoft.ContainerService/managedClusters/flexdemo"
        remote, _ = self.remote_script(config=cluster_id)
        result = subprocess.run(["bash", str(remote)], env=self.env, text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("already-joined", result.stdout)
        self.assertTrue(all(call[0] == "systemctl" for call in self.calls()))

    def test_remote_stopped_daemon_is_started_without_reinstall(self):
        cluster_id = f"/subscriptions/{SUBSCRIPTION}/resourceGroups/flexdemo/providers/Microsoft.ContainerService/managedClusters/flexdemo"
        remote, _ = self.remote_script(config=cluster_id)
        self.modify(host_active=False)
        for _ in range(2):
            result = subprocess.run(["bash", str(remote)], env=self.env, text=True, capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sum(call[:2] == ["systemctl", "start"] for call in self.calls()), 1)
        self.assertTrue(all(call[0] == "systemctl" for call in self.calls()))

    def test_remote_foreign_agent_is_not_reset(self):
        remote, _ = self.remote_script(config="https://another-cluster")
        result = subprocess.run(["bash", str(remote)], env=self.env, text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 78, result.stderr)
        self.assertIn("refusing reset", result.stderr)
        self.assertEqual(self.calls(), [])

    def test_remote_partial_bootstrap_is_not_replayed(self):
        remote, _ = self.remote_script(partial=True)
        result = subprocess.run(["bash", str(remote)], env=self.env, text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 78, result.stderr)
        self.assertIn("partial node deployment exists", result.stderr)
        self.assertEqual(self.calls(), [])

    def test_component_query_failure_is_not_treated_as_missing_workload(self):
        self.modify(fail_get="deployment/unbounded-net-controller")
        result = self.run_script(success=False)
        self.assertIn("cannot query deployment/unbounded-net-controller", result.stderr)
        self.assertEqual(self.state()["joined"], [])

    def test_legacy_short_pool_label_does_not_pass_flex_validation(self):
        self.modify(node_labels={"agentpool": "flexpool", "unbounded-cloud.io/site": "flex-site"})
        result = self.run_script(["--flex-count", "1"], success=False)
        self.assertIn("not assigned to the expected pool and network site", result.stderr)
        self.assertNotIn("Deployment ready:", result.stderr)

    def test_smoke_failure_is_not_reported_as_deployment_success(self):
        self.modify(fail_smoke_exec=True)
        result = self.run_script(["--flex-count", "1"], success=False)
        self.assertIn("smoke workload connection failed", result.stderr)
        self.assertNotIn("Deployment ready:", result.stderr)

    def test_foreign_smoke_namespace_is_not_adopted(self):
        self.modify(namespaces=[{"metadata": {"name": "aks-flex-smoke", "labels": {"unbounded-flex-cluster": "another-cluster"}}}])
        result = self.run_script(["--flex-count", "1"], success=False)
        self.assertIn("smoke namespace already exists and is not owned", result.stderr)

    def test_missing_saved_key_does_not_generate_replacement(self):
        self.run_script(["--flex-count", "1"])
        key = self.root / "state/id_ed25519"
        key.unlink()
        before = len(self.calls())
        result = self.run_script(["--flex-count", "1"], success=False)
        self.assertIn("saved SSH key is missing", result.stderr)
        self.assertFalse(key.exists())
        self.assertFalse(any("create" in call for call in self.calls()[before:]))

    def test_help_and_documented_deployment_examples_are_offline(self):
        help_result = subprocess.run(["bash", str(SCRIPT), "--help"], env=self.env, text=True, capture_output=True, timeout=5)
        self.assertEqual(help_result.returncode, 0, help_result.stderr)
        self.assertIn("run-command (default, Azure VM agent) or ssh", help_result.stdout)
        examples = []
        for document in (ROOT / "README.md", ROOT / "designs/aks-flex-cluster.md"):
            for block in re.findall(r"```bash\n(.*?)```", document.read_text(), re.DOTALL):
                if "bash hack/scripts/aks-flex-cluster.sh" in block:
                    command = block.replace("\\\n", "").replace("<subscription-uuid>", SUBSCRIPTION)
                    arguments = shlex.split(command)
                    arguments[1] = str(SCRIPT)
                    result = subprocess.run([*arguments, "--plan"], cwd=ROOT, env=self.env, text=True, capture_output=True, timeout=5)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("1 system + 3 Flex = 4 total", result.stdout)
                    self.assertIn("Bootstrap transport:      run-command", result.stdout)
                    examples.append(document.name)
        self.assertGreaterEqual(examples.count("README.md"), 1)
        self.assertGreaterEqual(examples.count("aks-flex-cluster.md"), 2)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.root / "state").exists())

    def test_make_target_does_not_require_go_during_makefile_parsing(self):
        result = subprocess.run(
            ["make", "--dry-run", "GOCMD=aks-flex-missing-go", "aks-flex-test"],
            cwd=ROOT, env=self.env, text=True, capture_output=True, timeout=5,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stderr, "")
        self.assertIn("bash -n hack/scripts/aks-flex-cluster.sh", result.stdout)
        self.assertIn("python3 hack/scripts/aks-flex-cluster_test.py", result.stdout)
        self.assertEqual(self.calls(), [])

    def test_invalid_parameters_fail_without_cloud_calls(self):
        for extra in (["--flex-count", "0"], ["--flex-count", "21"], ["--service-cidr", "10.91.0.0/24"],
                      ["--ssh-source-ip", "0.0.0.0/0"], ["--ssh-source-ip", "10.0.0.1"], ["--vnet-cidr", "10.91.1.1/16"],
                      ["--agent-version", "latest"], ["--timeout", "00"], ["--subscription", "not-a-uuid"],
                      ["--bootstrap-transport", "unsafe"]):
            with self.subTest(extra=extra):
                self.run_script(extra, success=False)
                self.assertEqual(self.calls(), [])

    def test_plan_is_offline_and_does_not_write_state(self):
        result = self.run_script(["--plan"])
        self.assertIn("1 system + 3 Flex = 4 total", result.stdout)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.root / "state").exists())

    def test_removed_acr_flags_fail_without_cloud_calls_or_state(self):
        for flag, value in (("--acr", "sharedtestacr"),
                            ("--acr-pull-identity", f"/subscriptions/{SUBSCRIPTION}/resourceGroups/shared/providers/Microsoft.ManagedIdentity/userAssignedIdentities/pull"),
                            ("--acr-probe-image", "sharedtestacr.azurecr.io/private/probe:v1")):
            for mode in ([], ["--plan"]):
                with self.subTest(flag=flag, mode=mode):
                    result = self.run_script([*mode, flag, value], success=False)
                    self.assertIn(f"unknown option: {flag}", result.stderr)
                    self.assertFalse((self.root / "state").exists())
        self.assertEqual(self.calls(), [])

    def test_stale_registry_intent_is_ignored_and_customer_auth_is_preserved(self):
        self.run_script(["--flex-count", "1"])
        state_dir = self.root / "state"
        legacy = state_dir / "registry-intent.json"
        legacy.write_text("obsolete state must not be parsed or changed\n")
        customer_auth = {"provider": "customer-owned", "credentials_path": "/customer/credentials.json"}
        self.modify(customer_auth=customer_auth)
        intent = (state_dir / "intent.json").read_bytes()
        self.run_script(["--flex-count", "1"])
        self.assertEqual(legacy.read_text(), "obsolete state must not be parsed or changed\n")
        self.assertEqual((state_dir / "intent.json").read_bytes(), intent)
        self.assertEqual(self.state()["customer_auth"], customer_auth)
        self.assertEqual([file.name for file in state_dir.glob("registry-*")], ["registry-intent.json"])
        self.assert_no_acr_wiring()

    def test_corrupt_cached_archive_is_rejected(self):
        self.run_script()
        archive = next((self.root / "state/cache").glob("*kubectl-unbounded*.tar.gz"))
        archive.write_bytes(b"not the published archive")
        result = self.run_script(success=False)
        self.assertIn("archive checksum mismatch", result.stderr)
        self.assertEqual(len(self.state()["joined"]), 3)

    def test_concurrent_run_does_not_remove_another_runs_lock(self):
        state = self.root / "state"
        state.mkdir()
        (state / ".lock").mkdir()
        result = self.run_script(success=False)
        self.assertIn("state directory is locked", result.stderr)
        self.assertTrue((state / ".lock").exists())
        self.assertEqual(self.calls()[0][0:2], ["az", "version"])
        self.assertFalse(any("create" in call for call in self.calls()))


if __name__ == "__main__":
    unittest.main(verbosity=2)
