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
        role_name = "dddddddd-1111-2222-3333-444444444444" if name == "Container Registry Repository Reader" else name
        out([] if s.get("missing_role") else [{"name":role_name,"roleType":"BuiltInRole"}])
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
    elif a[:2] == ["acr", "show"]:
        if s.get("registry_read_failure"): print("ACR forbidden",file=sys.stderr);sys.exit(1)
        out(s.get("registry",{"id":resource_id("Microsoft.ContainerRegistry/registries",name).replace("/flexdemo/","/shared/"),"name":name,"loginServer":name+".azurecr.io","roleAssignmentMode":"LegacyRegistryPermissions","tags":{"external":"preserve"}}))
    elif a[:2] == ["identity", "create"]:
        if name.endswith("-acr-pull"):
            if s.get("identity_create_failure"): print("Identity creation forbidden",file=sys.stderr);sys.exit(1)
            add("Microsoft.ManagedIdentity/userAssignedIdentities",name,principalId="aaaaaaaa-1111-2222-3333-444444444444",clientId="bbbbbbbb-1111-2222-3333-444444444444",tenantId="cccccccc-1111-2222-3333-444444444444")
        else: add("Microsoft.ManagedIdentity/userAssignedIdentities", name, principalId="controlplane-principal")
    elif a[:2] == ["identity", "show"]:
        if "--ids" in a: out(s["external_identity"])
        else: out(get("Microsoft.ManagedIdentity/userAssignedIdentities", name))
    elif a[:3] == ["identity", "federated-credential", "list"]:
        if s.get("federation_list_failure"): print("Federation query forbidden",file=sys.stderr);sys.exit(1)
        out([v for v in s.get("federation",[]) if v["identityName"] == arg("--identity-name")])
    elif a[:3] == ["identity", "federated-credential", "create"]:
        s.setdefault("federation",[]).append({"name":name,"identityName":arg("--identity-name"),"issuer":arg("--issuer"),"subject":arg("--subject"),"audiences":[arg("--audiences")]});save()
    elif a[:3] == ["role", "assignment", "list"]:
        out([r for r in s["roles"] if r["scope"] == arg("--scope")])
    elif a[:3] == ["role", "assignment", "create"]:
        if s.get("acr_role_failure") and arg("--role") != "4d97b98b-1d4f-4787-a291-c67834d212e7" and arg("--role") != "8f139b0f-7eaf-460b-a9da-5b1246d9ed0d":
            print("Registry role assignment forbidden",file=sys.stderr);sys.exit(1)
        s["roles"].append({"principalId":arg("--assignee-object-id"),"roleDefinitionId":"/roles/"+arg("--role"),"scope":arg("--scope")}); save()
    elif a[:2] == ["aks", "create"]:
        add("Microsoft.ContainerService/managedClusters", name, provisioningState="Succeeded",kubernetesVersion="1.36.2",currentKubernetesVersion="1.36.2",
            networkProfile={"networkPlugin":"none","serviceCidr":arg("--service-cidr"),"dnsServiceIp":arg("--dns-service-ip"),"podCidr":arg("--pod-cidr")},
            agentPoolProfiles=[{"name":"system","mode":"System","count":1,"vmSize":arg("--node-vm-size"),"vnetSubnetId":arg("--vnet-subnet-id")}])
    elif a[:2] == ["aks", "show"]: out(get("Microsoft.ContainerService/managedClusters", name))
    elif a[:2] == ["aks", "update"]:
        cluster=get("Microsoft.ContainerService/managedClusters",name)
        cluster["oidcIssuerProfile"]={"enabled":True,"issuerUrl":"https://issuer.example/flexdemo/"};save()
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
    elif "patch" in a:
        if s.get("registry_patch_failure"): print("ConfigMap patch forbidden",file=sys.stderr);sys.exit(1)
        patch=json.loads(Path(arg("--patch-file")).read_text())
        source=s["gantry_config"]
        assert patch[0]["value"] == source["metadata"]["resourceVersion"]
        source["metadata"]["annotations"]=patch[1]["value"]
        source["data"]["node-auth.yaml"]=patch[2]["value"]
        source["metadata"]["resourceVersion"]=str(int(source["metadata"]["resourceVersion"])+1);save();out("configmap/gantry-config")
    elif "exec" in a:
        if "registry-auth-node" in a:
            if s.get("unsupported_registry_operator"): print("unknown command",file=sys.stderr);sys.exit(1)
            out("--token-file projected infrastructure assertion");sys.exit(0)
        if s.get("fail_smoke_exec"):
            print("smoke workload connection failed",file=sys.stderr); sys.exit(1)
    elif "rollout" in a or "wait" in a or "logs" in a: pass
    elif "get" in a:
        kind = a[a.index("get")+1]
        if s.get("fail_get") == kind:
            print("Kubernetes API forbidden",file=sys.stderr); sys.exit(1)
        if kind == "configmap":
            if "gantry_config" not in s:
                s["gantry_config"]={"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"gantry-config","namespace":"unbounded-system","resourceVersion":"1","annotations":{"user.example/keep":"yes"}},"data":{"config.yaml":"# preserve administrator settings\nmirror_listen: 0.0.0.0:5000\nupstream_registries:\n- name: registry.example.com\n  endpoint: https://registry.example.com\n","unrelated":"preserve"}};save()
            out(s["gantry_config"])
        elif kind == "namespaces": out({"items":s.get("namespaces",[])})
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
                      ["--bootstrap-transport", "unsafe"], ["--acr", "https://sharedtestacr.azurecr.io"],
                      ["--acr-pull-identity", "not-a-resource-id"], ["--acr-probe-image", "sharedtestacr.azurecr.io/repo:v1"],
                      ["--acr", "sharedtestacr", "--acr-probe-image", "https://unsafe.example/repo?secret=1"]):
            with self.subTest(extra=extra):
                self.run_script(extra, success=False)
                self.assertEqual(self.calls(), [])

    def test_plan_is_offline_and_does_not_write_state(self):
        result = self.run_script(["--plan"])
        self.assertIn("1 system + 3 Flex = 4 total", result.stdout)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.root / "state").exists())

    def test_shared_acr_uses_one_federated_cluster_identity(self):
        options = ["--acr", "sharedtestacr", "--acr-probe-image", "sharedtestacr.azurecr.io/private/probe:v1"]
        self.run_script(options)
        state = self.state()
        identity = state["resources"]["Microsoft.ManagedIdentity/userAssignedIdentities/flexdemo-acr-pull"]
        pulls = [role for role in state["roles"] if role["roleDefinitionId"].endswith("/7f951dda-4ed3-4680-a7ca-43fe172d538d")]
        self.assertEqual(len(pulls), 1)
        self.assertEqual(pulls[0]["principalId"], identity["principalId"])
        self.assertIn("/resourceGroups/shared/", pulls[0]["scope"])
        self.assertFalse(any(role["principalId"].startswith("flexdemo-flex") for role in pulls))
        self.assertFalse(any(resource["type"] == "Microsoft.ContainerRegistry/registries" for resource in state["resources"].values()))
        self.assertEqual(len(state["federation"]), 1)
        trust = state["federation"][0]
        self.assertEqual(trust["subject"], "system:serviceaccount:unbounded-system:registry-pull")
        self.assertEqual(trust["issuer"], "https://issuer.example/flexdemo/")
        self.assertEqual(trust["audiences"], ["api://AzureADTokenExchange"])
        config = state["gantry_config"]
        policy = json.loads(config["data"]["node-auth.yaml"])
        self.assertEqual(policy, {"mode": "workloadIdentity", "registry": "sharedtestacr.azurecr.io", "clientID": identity["clientId"], "tenantID": identity["tenantId"], "probeImage": options[-1]})
        self.assertIn("preserve administrator settings", config["data"]["config.yaml"])
        self.assertEqual(config["data"]["unrelated"], "preserve")
        self.assertEqual(config["metadata"]["annotations"]["user.example/keep"], "yes")
        smoke = json.loads((self.root / "state/smoke.json").read_text())["items"][1]["spec"]["template"]["spec"]
        self.assertNotIn("serviceAccountName", smoke)
        self.assertNotIn("imagePullSecrets", smoke)
        before = len(self.calls())
        self.run_script()  # Saved registry intent survives an ordinary rerun.
        self.assertFalse(any(call[0] == "az" and "create" in call for call in self.calls()[before:]))
        self.assertEqual(len(self.state()["roles"]), 5)
        self.assertEqual(len(self.state()["federation"]), 1)
        record = json.loads((self.root / "state/registry-access.json").read_text())
        self.assertTrue(record["identityCreatedByDeployment"])
        self.assertNotIn("accessToken", record)

    def test_acr_can_be_onboarded_after_cluster_creation(self):
        self.run_script()
        intent = (self.root / "state/intent.json").read_bytes()
        before = self.state()
        self.run_script(["--acr", "sharedtestacr"])
        after = self.state()
        self.assertEqual(intent, (self.root / "state/intent.json").read_bytes())
        self.assertEqual(before["bootstraps"], after["bootstraps"])
        for name, resource in before["resources"].items():
            if resource["type"] != "Microsoft.ContainerService/managedClusters":
                self.assertEqual(resource, after["resources"][name])
        self.assertTrue(after["resources"]["Microsoft.ContainerService/managedClusters/flexdemo"]["oidcIssuerProfile"]["enabled"])
        self.assertEqual(sum(call[:3] == ["az", "aks", "update"] for call in self.calls()), 1)

    def test_existing_pull_identity_is_resolved_without_adoption(self):
        identity = {"id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/shared-pull", "principalId": "aaaaaaaa-1111-2222-3333-444444444444", "clientId": "bbbbbbbb-1111-2222-3333-444444444444", "tenantId": "cccccccc-1111-2222-3333-444444444444", "tags": {"foreign": "preserve"}}
        self.modify(external_identity=identity)
        self.run_script(["--acr", "sharedtestacr", "--acr-pull-identity", identity["id"]])
        self.assertEqual(self.state()["external_identity"], identity)
        self.assertNotIn("Microsoft.ManagedIdentity/userAssignedIdentities/flexdemo-acr-pull", self.state()["resources"])
        record = json.loads((self.root / "state/registry-access.json").read_text())
        self.assertFalse(record["identityCreatedByDeployment"])
        self.assertEqual(record["identityResourceId"], identity["id"])
        self.assertEqual(self.state()["federation"][0]["identityName"], "shared-pull")

    def test_matching_preconfigured_federation_is_reused_by_content(self):
        self.run_script(["--acr", "sharedtestacr", "--flex-count", "1"])
        trust = self.state()["federation"]
        trust[0]["name"] = "platform-managed-trust"
        self.modify(federation=trust)
        before = len(self.calls())
        self.run_script(["--flex-count", "1"])
        self.assertEqual(self.state()["federation"], trust)
        self.assertFalse(any(call[:4] == ["az", "identity", "federated-credential", "create"] for call in self.calls()[before:]))
        record = json.loads((self.root / "state/registry-access.json").read_text())
        self.assertEqual(record["federatedCredentialName"], "platform-managed-trust")
        self.assertFalse(record["federationManagedByDeployment"])

    def test_artifact_streaming_runner_fails_before_cloud_calls_without_its_variant(self):
        runner = ROOT / "hack/scripts/gantry-aks-integration.sh"
        result = subprocess.run(["bash", "-c", 'source "$1"; COMMAND=deploy; REPO_ROOT="$2"; initialize', "test", str(runner), str(self.root)], env=self.env, text=True, capture_output=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifact-streaming Gantry image/chart support is absent", result.stderr)
        self.assertEqual(self.calls(), [])

    def test_registry_auth_requires_a_compatible_operator_before_iam_changes(self):
        self.modify(unsupported_registry_operator=True)
        result = self.run_script(["--acr", "sharedtestacr"], success=False)
        self.assertIn("does not support federated registry onboarding", result.stderr)
        self.assertNotIn("Microsoft.ManagedIdentity/userAssignedIdentities/flexdemo-acr-pull", self.state()["resources"])
        self.assertFalse(any("7f951dda" in role["roleDefinitionId"] for role in self.state()["roles"]))

    def test_foreign_registry_policy_is_not_overwritten(self):
        source = {"metadata": {"resourceVersion": "1", "annotations": {}}, "data": {"config.yaml": "upstream_registries: []", "node-auth.yaml": "mode: external"}}
        self.modify(gantry_config=source)
        result = self.run_script(["--acr", "sharedtestacr"], success=False)
        self.assertIn("not owned by this deployment", result.stderr)
        self.assertEqual(self.state()["gantry_config"], source)
        self.assertNotIn("Microsoft.ManagedIdentity/userAssignedIdentities/flexdemo-acr-pull", self.state()["resources"])

    def test_conflicting_federation_is_not_overwritten(self):
        self.run_script(["--acr", "sharedtestacr"])
        federation = self.state()["federation"]
        federation[0]["subject"] = "system:serviceaccount:another:account"
        self.modify(federation=federation)
        before = len(self.calls())
        result = self.run_script(success=False)
        self.assertIn("federation binding conflicts", result.stderr)
        self.assertEqual(self.state()["federation"], federation)
        self.assertFalse(any(call[:4] == ["az", "identity", "federated-credential", "create"] for call in self.calls()[before:]))

    def test_registry_permission_failures_never_use_admin_fallback(self):
        for failure in ("registry_read_failure", "identity_create_failure", "federation_list_failure", "acr_role_failure", "registry_patch_failure"):
            with self.subTest(failure=failure):
                self.modify(**{failure: True})
                result = self.run_script(["--acr", "sharedtestacr", "--flex-count", "1"], success=False)
                self.assertNotIn("Deployment ready:", result.stderr)
                self.assertFalse(any(call[0] == "az" and "--role" in call and call[call.index("--role") + 1] in ("Owner", "Contributor", "User Access Administrator") for call in self.calls()))
                self.modify(**{failure: False})
        self.run_script(["--acr", "sharedtestacr", "--flex-count", "1"])
        self.assertEqual(len(self.state()["federation"]), 1)

    def test_abac_registry_uses_repository_reader_role(self):
        registry = {"id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/shared/providers/Microsoft.ContainerRegistry/registries/sharedtestacr", "loginServer": "sharedtestacr.azurecr.io", "roleAssignmentMode": "AbacRepositoryPermissions"}
        self.modify(registry=registry)
        self.run_script(["--acr", "sharedtestacr", "--flex-count", "1"])
        self.assertEqual(self.state()["registry"], registry)
        self.assertTrue(any(role["roleDefinitionId"].endswith("/dddddddd-1111-2222-3333-444444444444") for role in self.state()["roles"]))
        self.assertFalse(any(role["roleDefinitionId"].endswith("/7f951dda-4ed3-4680-a7ca-43fe172d538d") for role in self.state()["roles"]))

    def test_registry_changes_require_explicit_reconfiguration(self):
        self.run_script(["--acr", "sharedtestacr", "--flex-count", "1"])
        before = len(self.calls())
        result = self.run_script(["--acr", "anotherregistry", "--flex-count", "1"], success=False)
        self.assertIn("differs from saved intent", result.stderr)
        self.assertFalse(any(call[0] == "az" and "create" in call for call in self.calls()[before:]))

    def test_registry_plan_remains_offline(self):
        result = self.run_script(["--acr", "sharedtestacr", "--plan"])
        self.assertIn("one identity per cluster", result.stdout)
        self.assertIn("system:serviceaccount:unbounded-system:registry-pull", result.stdout)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.root / "state").exists())

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
