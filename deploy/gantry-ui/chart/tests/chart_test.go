// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type resource struct {
	Kind     string                `json:"kind"`
	Metadata metav1.ObjectMeta     `json:"metadata"`
	Spec     appsv1.DeploymentSpec `json:"spec"`
	Rules    []rbacv1.PolicyRule   `json:"rules"`
	RoleRef  rbacv1.RoleRef        `json:"roleRef"`
	Subjects []rbacv1.Subject      `json:"subjects"`
}

func helm(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()

	binary := os.Getenv("HELM_BIN")
	if binary == "" {
		var err error

		binary, err = exec.LookPath("helm")
		if err != nil {
			t.Skip("helm not on PATH")
		}
	}

	return exec.Command(binary, args...).CombinedOutput()
}

func values(overrides ...string) []string {
	settings := []string{
		"image.digest=" + digest,
		"registry=example.azurecr.io",
		"azure.clientID=dashboard-client",
	}
	settings = append(settings, overrides...)

	var args []string
	for _, setting := range settings {
		args = append(args, "--set", setting)
	}

	return args
}

func render(t *testing.T, release, namespace string, overrides ...string) []resource {
	t.Helper()

	args := append([]string{"template", release, "..", "--namespace", namespace}, values(overrides...)...)

	output, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)

	var resources []resource

	for {
		var item resource

		err := decoder.Decode(&item)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("decode Helm output: %v\n%s", err, output)
		}

		if item.Kind != "" {
			resources = append(resources, item)
		}
	}

	return resources
}

func find(t *testing.T, resources []resource, kind, name string) resource {
	t.Helper()

	for _, item := range resources {
		if item.Kind == kind && (name == "" || item.Metadata.Name == name) {
			return item
		}
	}

	t.Fatalf("%s %q not found", kind, name)

	return resource{}
}

func hasRule(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) {
			return true
		}
	}

	return false
}

func TestDashboardDefaults(t *testing.T) {
	resources := render(t, "dashboard", "gantry-demo")
	deployment := find(t, resources, "Deployment", "")
	pod := deployment.Spec.Template
	container := pod.Spec.Containers[0]
	name := deployment.Metadata.Name

	if *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("expected one replica with Recreate updates")
	}

	if !pod.Spec.HostNetwork || pod.Spec.NodeSelector["kubernetes.io/os"] != "linux" ||
		pod.Spec.NodeSelector["kubernetes.azure.com/mode"] != "system" {
		t.Fatal("dashboard must use a Linux system node's host network")
	}

	for _, arg := range []string{
		"--listen=127.0.0.1:8088", "--registry=example.azurecr.io", "--namespace=gantry-demo",
		"--gantry-namespace=unbounded-system", "--node-pool=flexpool", "--architecture=amd64",
		"--direct-agents", "--interval=1s",
	} {
		if !slices.Contains(container.Args, arg) {
			t.Errorf("missing dashboard argument %s", arg)
		}
	}

	for _, arg := range container.Args {
		if strings.HasPrefix(arg, "--builder-") || strings.HasPrefix(arg, "--reclaimer-") || strings.HasPrefix(arg, "--helper-") {
			t.Errorf("helper enabled by default: %s", arg)
		}
	}

	if container.Image != "ghcr.io/vpatelsj/gantry-ui@"+digest || len(pod.Spec.ImagePullSecrets) != 0 {
		t.Fatal("unexpected dashboard image or pull secrets")
	}

	security := pod.Spec.SecurityContext
	if !*security.RunAsNonRoot || *security.RunAsUser != 65532 || *security.RunAsGroup != 65532 ||
		security.SeccompProfile.Type != "RuntimeDefault" || *container.SecurityContext.AllowPrivilegeEscalation ||
		!*container.SecurityContext.ReadOnlyRootFilesystem ||
		len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("dashboard security context regressed")
	}

	if container.ReadinessProbe.HTTPGet.Host != "127.0.0.1" || container.LivenessProbe.HTTPGet.Host != "127.0.0.1" ||
		container.ReadinessProbe.HTTPGet.Port.IntVal != 8088 || container.LivenessProbe.HTTPGet.Port.IntVal != 8088 {
		t.Fatal("probes must use the node loopback listener")
	}

	account := find(t, resources, "ServiceAccount", name)
	if account.Metadata.Annotations["azure.workload.identity/client-id"] != "dashboard-client" ||
		pod.Labels["azure.workload.identity/use"] != "true" || pod.Spec.ServiceAccountName != name {
		t.Fatal("dashboard workload identity is not wired to its dedicated account")
	}

	env := map[string]string{}
	for _, value := range container.Env {
		env[value.Name] = value.Value
	}

	if env["AZURE_CLIENT_ID"] != "dashboard-client" || env["AZURE_TOKEN_CREDENTIALS"] != "WorkloadIdentityCredential" {
		t.Fatal("dashboard must select workload identity, not host IMDS")
	}

	accountCount := 0

	for _, item := range resources {
		if item.Kind == "Service" || item.Kind == "Ingress" {
			t.Fatal("dashboard must not publish a Service or Ingress")
		}

		if item.Kind == "ServiceAccount" {
			accountCount++
		}

		for _, rule := range item.Rules {
			if slices.Contains(rule.Verbs, "delete") || slices.Contains(rule.Verbs, "*") {
				t.Fatal("default RBAC must not grant deletion or wildcard verbs")
			}
		}
	}

	if accountCount != 1 {
		t.Fatalf("default account count = %d, want 1", accountCount)
	}

	role := find(t, resources, "Role", name)
	for _, permission := range [][3]string{
		{"apps", "daemonsets", "get"},
		{"apps", "daemonsets", "list"},
		{"apps", "daemonsets", "create"},
		{"", "pods", "list"},
		{"", "events", "list"},
		{"", "pods/log", "get"},
		{"batch", "jobs", "get"},
		{"batch", "jobs", "list"},
		{"batch", "jobs", "create"},
	} {
		if !hasRule(role.Rules, permission[0], permission[1], permission[2]) {
			t.Errorf("missing dashboard RBAC %v", permission)
		}
	}

	agents := find(t, resources, "Role", name+"-agents")
	if agents.Metadata.Namespace != "unbounded-system" || len(agents.Rules) != 1 ||
		!hasRule(agents.Rules, "", "pods", "list") || len(agents.Rules[0].Verbs) != 1 {
		t.Fatal("cross-namespace agent RBAC must only list pods")
	}

	nodes := find(t, resources, "ClusterRole", name+"-nodes")
	if len(nodes.Rules) != 1 || !hasRule(nodes.Rules, "", "nodes", "list") || len(nodes.Rules[0].Verbs) != 1 {
		t.Fatal("cluster RBAC must only list nodes")
	}
}

func TestOptionalHelpersAndPullSecrets(t *testing.T) {
	resources := render(t, "dashboard", "gantry-demo",
		"builder.enabled=true", "builder.azure.clientID=builder-client",
		"builder.base=example.azurecr.io/base@"+digest, "builder.repository=demo/generated", "builder.concurrency=2",
		"reclaimer.enabled=true", "imagePullSecrets[0].name=dashboard-first",
		"imagePullSecrets[1].name=dashboard-second", "helperImagePullSecret=helper-pull",
		"nodeSelector.kubernetes\\.io/hostname=system-node", "nodePool=otherpool",
		"architecture=arm64", "gantryNamespace=agents", "interval=2s")
	deployment := find(t, resources, "Deployment", "")
	name := deployment.Metadata.Name
	pod := deployment.Spec.Template.Spec
	container := pod.Containers[0]

	for _, arg := range []string{
		"--builder-image=" + container.Image, "--reclaimer-image=" + container.Image,
		"--builder-service-account=" + name + "-builder", "--reclaimer-service-account=" + name + "-reclaimer",
		"--builder-base=example.azurecr.io/base@" + digest, "--builder-repository=demo/generated", "--builder-concurrency=2",
		"--helper-image-pull-secret=helper-pull", "--node-pool=otherpool", "--architecture=arm64", "--gantry-namespace=agents", "--interval=2s",
	} {
		if !slices.Contains(container.Args, arg) {
			t.Errorf("missing helper/config argument %s", arg)
		}
	}

	if len(pod.ImagePullSecrets) != 2 || pod.ImagePullSecrets[0].Name != "dashboard-first" ||
		pod.ImagePullSecrets[1].Name != "dashboard-second" || pod.NodeSelector["kubernetes.io/hostname"] != "system-node" {
		t.Fatal("dashboard pull secrets or node selector missing")
	}

	builder := find(t, resources, "ServiceAccount", name+"-builder")
	if builder.Metadata.Annotations["azure.workload.identity/client-id"] != "builder-client" {
		t.Fatal("builder requires its own workload identity")
	}

	builderRole := find(t, resources, "Role", name+"-builder")
	if len(builderRole.Rules) != 1 || !hasRule(builderRole.Rules, "apps", "daemonsets", "create") ||
		len(builderRole.Rules[0].Verbs) != 1 {
		t.Fatal("builder RBAC must only create daemonsets")
	}

	reclaimerRole := find(t, resources, "Role", name+"-reclaimer")
	for _, permission := range [][3]string{
		{"apps", "daemonsets", "list"},
		{"apps", "daemonsets", "delete"},
		{"", "pods", "list"},
		{"batch", "jobs", "get"},
		{"batch", "jobs", "list"},
		{"batch", "jobs", "create"},
	} {
		if !hasRule(reclaimerRole.Rules, permission[0], permission[1], permission[2]) {
			t.Errorf("missing reclaimer RBAC %v", permission)
		}
	}

	reclaimerNodes := find(t, resources, "ClusterRole", name+"-reclaimer-nodes")
	if !hasRule(reclaimerNodes.Rules, "", "nodes", "list") {
		t.Fatal("reclaimer must list nodes")
	}

	role := find(t, resources, "Role", name)
	deleteRules := 0

	for _, rule := range role.Rules {
		if slices.Contains(rule.Verbs, "delete") {
			deleteRules++

			if !slices.Equal(rule.APIGroups, []string{"batch"}) || !slices.Equal(rule.Resources, []string{"jobs"}) ||
				!slices.Equal(rule.ResourceNames, []string{"gantry-reclaim"}) || !slices.Equal(rule.Verbs, []string{"delete"}) {
				t.Fatal("dashboard deletion must be restricted to the fixed reclaim Job")
			}
		}
	}

	if deleteRules != 1 {
		t.Fatal("enabled reclaimer must allow the dashboard to retry its fixed Job")
	}

	agents := find(t, resources, "Role", name+"-agents")
	if agents.Metadata.Namespace != "agents" {
		t.Fatal("configured Gantry namespace was ignored")
	}

	for _, item := range resources {
		if item.Kind != "RoleBinding" && item.Kind != "ClusterRoleBinding" {
			continue
		}

		for _, subject := range item.Subjects {
			if subject.Kind != "ServiceAccount" || subject.Namespace != "gantry-demo" ||
				!strings.HasPrefix(subject.Name, name) {
				t.Errorf("binding %s has an incorrectly scoped subject: %+v", item.Metadata.Name, subject)
			}
		}

		find(t, resources, item.RoleRef.Kind, item.RoleRef.Name)
	}
}

func TestResourceNamesAreReleaseAndNamespaceScoped(t *testing.T) {
	longName := strings.Repeat("a", 53)
	first := render(t, longName, "first", "reclaimer.enabled=true")
	second := render(t, longName, "second", "reclaimer.enabled=true")
	third := render(t, strings.Repeat("a", 52)+"b", "first", "reclaimer.enabled=true")

	for _, other := range [][]resource{second, third} {
		for _, left := range first {
			if len(left.Metadata.Name) > 63 {
				t.Errorf("resource name exceeds DNS label size: %s", left.Metadata.Name)
			}

			for _, right := range other {
				if left.Kind == right.Kind && left.Metadata.Name == right.Metadata.Name {
					t.Errorf("release/namespace collision: %s %s", left.Kind, left.Metadata.Name)
				}
			}
		}
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings []string
	}{
		{name: "missing digest", settings: []string{"image.digest="}},
		{name: "tag instead of digest", settings: []string{"image.digest=latest"}},
		{name: "short digest", settings: []string{"image.digest=sha256:abc"}},
		{name: "nonhex digest", settings: []string{"image.digest=sha256:" + strings.Repeat("z", 64)}},
		{name: "tag in image repository", settings: []string{"image.repository=ghcr.io/vpatelsj/gantry-ui:latest"}},
		{name: "missing registry", settings: []string{"registry="}},
		{name: "not ACR", settings: []string{"registry=registry.example.com"}},
		{name: "missing dashboard identity", settings: []string{"azure.clientID="}},
		{name: "missing builder identity", settings: []string{"builder.enabled=true", "builder.base=example.azurecr.io/base@" + digest}},
		{name: "missing builder base", settings: []string{"builder.enabled=true", "builder.azure.clientID=builder-client"}},
		{name: "mutable builder base", settings: []string{"builder.enabled=true", "builder.azure.clientID=builder-client", "builder.base=example.azurecr.io/base:latest"}},
		{name: "wrong base registry", settings: []string{"builder.enabled=true", "builder.azure.clientID=builder-client", "builder.base=another.azurecr.io/base@" + digest}},
		{name: "zero concurrency", settings: []string{"builder.concurrency=0"}},
		{name: "unsupported architecture", settings: []string{"architecture=ppc64le"}},
		{name: "non Linux dashboard", settings: []string{"nodeSelector.kubernetes\\.io/os=windows"}},
		{name: "invalid pull secret name", settings: []string{"imagePullSecrets[0].name=invalid/name"}},
		{name: "invalid helper pull secret", settings: []string{"helperImagePullSecret=invalid/name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "dashboard", "..", "--namespace", "gantry-demo"}, values(tc.settings...)...)

			output, err := helm(t, args...)
			if err == nil {
				t.Fatalf("invalid chart values accepted:\n%s", output)
			}
		})
	}
}

func TestChartLintAndPortForwardNotes(t *testing.T) {
	args := append([]string{"lint", "..", "--strict"}, values()...)

	output, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm lint: %v\n%s", err, output)
	}

	resources := render(t, "dashboard", "gantry-demo")
	name := find(t, resources, "Deployment", "").Metadata.Name

	notes, err := os.ReadFile("../templates/NOTES.txt")
	if err != nil {
		t.Fatal(err)
	}

	expanded := strings.NewReplacer(
		"{{ .Release.Namespace }}", "gantry-demo",
		`{{ include "gantry-ui.fullname" . }}`, name,
	).Replace(string(notes))

	command := "kubectl --namespace gantry-demo port-forward --address 127.0.0.1 deployment/" + name + " 8088:8088"
	if !strings.Contains(expanded, command) {
		t.Fatalf("NOTES missing the correct loopback Deployment port-forward: %s", expanded)
	}
}
