// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundedoperator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator"
)

// TestOperatorConfigEndpointAndHashRender asserts the make/GitOps render path
// (`--set APIServerEndpoint=`) both populates the operator-config ConfigMap
// endpoint and stamps a matching config-hash annotation on the Deployment pod
// template. Together these make a re-render with a changed endpoint roll the
// operator (envFrom is read once at pod start), and the shared hash keeps the
// two templates from drifting apart (findings #3/#4).
func TestOperatorConfigEndpointAndHashRender(t *testing.T) {
	t.Parallel()

	const endpoint = `https://api.example.test:6443/path?mode="strict"&ready=true`

	templatesDir := filepath.Join(repoRoot(t), "deploy", "unbounded-operator")
	outputDir := t.TempDir()

	data := map[string]string{
		"Namespace":         "unbounded-system",
		"OperatorImage":     "operator:test",
		"ImageRegistry":     "registry.example.com/mirror",
		"APIServerEndpoint": endpoint,
	}
	if err := render.Render(templatesDir, outputDir, data); err != nil {
		t.Fatalf("render.Render: %v", err)
	}

	// The ConfigMap carries the endpoint that the operator reads via envFrom.
	var cm struct {
		Data map[string]string `yaml:"data"`
	}

	readYAML(t, filepath.Join(outputDir, "03-configmap.yaml"), &cm)

	if got := cm.Data["UNBOUNDED_API_SERVER_ENDPOINT"]; got != endpoint {
		t.Fatalf("configmap UNBOUNDED_API_SERVER_ENDPOINT = %q, want %q", got, endpoint)
	}

	if got := cm.Data["UNBOUNDED_IMAGE_REGISTRY"]; got != "registry.example.com/mirror" {
		t.Fatalf("configmap UNBOUNDED_IMAGE_REGISTRY = %q", got)
	}

	// The Deployment pod template carries the matching hash annotation.
	var deploy struct {
		Spec struct {
			Replicas int32          `yaml:"replicas"`
			Strategy map[string]any `yaml:"strategy"`
			Template struct {
				Metadata struct {
					Labels      map[string]string `yaml:"labels"`
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					HostNetwork bool   `yaml:"hostNetwork"`
					DNSPolicy   string `yaml:"dnsPolicy"`
					Tolerations []struct {
						Key      string `yaml:"key"`
						Operator string `yaml:"operator"`
						Effect   string `yaml:"effect"`
					} `yaml:"tolerations"`
					Containers []struct {
						Name         string   `yaml:"name"`
						Args         []string `yaml:"args"`
						StartupProbe *struct {
							PeriodSeconds    int32 `yaml:"periodSeconds"`
							FailureThreshold int32 `yaml:"failureThreshold"`
						} `yaml:"startupProbe"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	readYAML(t, filepath.Join(outputDir, "04-deployment.yaml"), &deploy)

	canonical, err := json.Marshal(cm.Data)
	if err != nil {
		t.Fatalf("marshal configmap data: %v", err)
	}

	sum := sha256.Sum256(canonical)
	want := hex.EncodeToString(sum[:])

	if got := deploy.Spec.Template.Metadata.Annotations["unbounded-cloud.io/operator-config-hash"]; got != want {
		t.Fatalf("deployment operator-config-hash annotation = %q, want %q (sha256 of complete ConfigMap data)", got, want)
	}

	// The pod carries the AKS FQDN annotation so KUBERNETES_SERVICE_HOST resolves
	// to the public API FQDN, the last-resort source for the advertised endpoint.
	// AKS only honors this as an annotation (not a label).
	if got := deploy.Spec.Template.Metadata.Annotations["kubernetes.azure.com/set-kube-service-host-fqdn"]; got != "true" {
		t.Fatalf("deployment pod annotation kubernetes.azure.com/set-kube-service-host-fqdn = %q, want \"true\"", got)
	}

	if deploy.Spec.Replicas != 1 {
		t.Fatalf("deployment replicas = %d, want 1", deploy.Spec.Replicas)
	}

	if got := deploy.Spec.Strategy["type"]; got != "Recreate" {
		t.Fatalf("deployment strategy = %q, want Recreate", got)
	}

	rollingUpdate, found := deploy.Spec.Strategy["rollingUpdate"]
	if !found || rollingUpdate != nil {
		t.Fatalf("deployment strategy rollingUpdate = %#v (present: %t), want explicit null", rollingUpdate, found)
	}

	if !deploy.Spec.Template.Spec.HostNetwork {
		t.Fatal("deployment pod hostNetwork = false, want true so the operator can install CNI")
	}

	if got := deploy.Spec.Template.Spec.DNSPolicy; got != "Default" {
		t.Fatalf("deployment pod dnsPolicy = %q, want Default for pre-CNI API FQDN resolution", got)
	}

	foundNotReadyToleration := false

	for _, toleration := range deploy.Spec.Template.Spec.Tolerations {
		if toleration.Key == "node.kubernetes.io/not-ready" && toleration.Operator == "Exists" && toleration.Effect == "NoSchedule" {
			foundNotReadyToleration = true

			break
		}
	}

	if !foundNotReadyToleration {
		t.Fatalf("deployment pod tolerations = %#v, want node.kubernetes.io/not-ready Exists NoSchedule", deploy.Spec.Template.Spec.Tolerations)
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name != "controller" || container.StartupProbe == nil {
			continue
		}

		if !contains(container.Args, "--leader-elect=true") {
			t.Fatalf("controller args = %v, want --leader-elect=true", container.Args)
		}

		budget := time.Duration(container.StartupProbe.FailureThreshold) *
			time.Duration(container.StartupProbe.PeriodSeconds) * time.Second
		if budget <= operator.CRDBootstrapTimeout {
			t.Fatalf("startup probe budget = %s, must exceed CRD bootstrap timeout %s", budget, operator.CRDBootstrapTimeout)
		}

		return
	}

	t.Fatal("controller startup probe not found")
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func TestOperatorConfigHashChangesWithEachConfigValue(t *testing.T) {
	t.Parallel()

	renderHash := func(endpoint, registry, reapLegacyResources string) string {
		t.Helper()

		outputDir := t.TempDir()
		if err := render.Render(filepath.Join(repoRoot(t), "deploy", "unbounded-operator"), outputDir, map[string]string{
			"Namespace":           "unbounded-system",
			"OperatorImage":       "operator:test",
			"ImageRegistry":       registry,
			"APIServerEndpoint":   endpoint,
			"ReapLegacyResources": reapLegacyResources,
		}); err != nil {
			t.Fatalf("render.Render: %v", err)
		}

		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		readYAML(t, filepath.Join(outputDir, "03-configmap.yaml"), &cm)

		if got := cm.Data["UNBOUNDED_API_SERVER_ENDPOINT"]; got != endpoint {
			t.Fatalf("configmap endpoint = %q, want %q", got, endpoint)
		}

		if got := cm.Data["UNBOUNDED_REAP_LEGACY_RESOURCES"]; got != reapLegacyResources {
			t.Fatalf("configmap reap legacy resources = %q, want %q", got, reapLegacyResources)
		}

		var deploy struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Annotations map[string]string `yaml:"annotations"`
					} `yaml:"metadata"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		readYAML(t, filepath.Join(outputDir, "04-deployment.yaml"), &deploy)

		canonical, err := json.Marshal(cm.Data)
		if err != nil {
			t.Fatalf("marshal configmap data: %v", err)
		}

		sum := sha256.Sum256(canonical)
		wantHash := hex.EncodeToString(sum[:])

		gotHash := deploy.Spec.Template.Metadata.Annotations["unbounded-cloud.io/operator-config-hash"]
		if gotHash != wantHash {
			t.Fatalf("deployment config hash = %q, want %q", gotHash, wantHash)
		}

		return gotHash
	}

	baseline := renderHash("https://api.example.test:6443", "ghcr.io", "true")
	if got := renderHash("https://other.example.test:6443", "ghcr.io", "true"); got == baseline {
		t.Fatal("changing UNBOUNDED_API_SERVER_ENDPOINT did not change the rendered config hash")
	}

	if got := renderHash("https://api.example.test:6443", "ghcr.io", "false"); got == baseline {
		t.Fatal("changing UNBOUNDED_REAP_LEGACY_RESOURCES did not change the rendered config hash")
	}

	if got := renderHash("https://api.example.test:6443", "registry.example.com", "true"); got == baseline {
		t.Fatal("changing UNBOUNDED_IMAGE_REGISTRY did not change the rendered config hash")
	}
}

func TestOperatorRBACIncludesCachedReadKinds(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	if err := render.Render(filepath.Join(repoRoot(t), "deploy", "unbounded-operator"), outputDir, map[string]string{
		"Namespace":     "unbounded-system",
		"OperatorImage": "operator:test",
		"ImageRegistry": "ghcr.io",
	}); err != nil {
		t.Fatalf("render.Render: %v", err)
	}

	var role struct {
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	readYAML(t, filepath.Join(outputDir, "02-rbac.yaml"), &role)

	assertReadOnlyRule := func(group, resource string) {
		t.Helper()

		for _, rule := range role.Rules {
			if !contains(rule.APIGroups, group) || !contains(rule.Resources, resource) {
				continue
			}

			if len(rule.Verbs) != 3 || rule.Verbs[0] != "get" || rule.Verbs[1] != "list" || rule.Verbs[2] != "watch" {
				t.Fatalf("%s/%s verbs = %v, want [get list watch]", group, resource, rule.Verbs)
			}

			return
		}

		t.Fatalf("read-only RBAC rule for %s/%s not found", group, resource)
	}

	assertReadOnlyRule("apps", "statefulsets")
	assertReadOnlyRule("", "nodes")
	assertReadOnlyRule("net.unbounded-cloud.io", "sitenodeslices")

	for _, rule := range role.Rules {
		if len(rule.APIGroups) == 1 && rule.APIGroups[0] == "events.k8s.io" &&
			len(rule.Resources) == 1 && rule.Resources[0] == "events" &&
			len(rule.Verbs) == 3 && rule.Verbs[0] == "create" && rule.Verbs[1] == "patch" && rule.Verbs[2] == "update" {
			return
		}
	}

	t.Fatal("events.k8s.io Event writer RBAC rule not found")
}

// TestOperatorImageAndComponentRegistryShareRegistry guards against the #574
// regression where the operator's own image and the component image-registry
// came from two unrelated defaults that only agreed because the org was literally
// "azure". The operator resolves component images as
// <UNBOUNDED_IMAGE_REGISTRY>/<component>:<tag>, so the rendered operator image
// must be exactly <UNBOUNDED_IMAGE_REGISTRY>/unbounded-operator:<tag>. Rendering
// with the template's default ImageRegistry (OperatorImage supplied as the
// make/release path does) asserts the two share a registry and cannot drift.
func TestOperatorImageAndComponentRegistryShareRegistry(t *testing.T) {
	t.Parallel()

	const (
		tag           = "v1.2.3"
		operatorImage = "ghcr.io/azure/unbounded-operator:" + tag
	)

	outputDir := t.TempDir()
	// Omit ImageRegistry so the ConfigMap default is exercised; supply
	// OperatorImage as the make/release render path does.
	if err := render.Render(filepath.Join(repoRoot(t), "deploy", "unbounded-operator"), outputDir, map[string]string{
		"Namespace":     "unbounded-system",
		"OperatorImage": operatorImage,
	}); err != nil {
		t.Fatalf("render.Render: %v", err)
	}

	var cm struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	readYAML(t, filepath.Join(outputDir, "03-configmap.yaml"), &cm)

	registry := cm.Data["UNBOUNDED_IMAGE_REGISTRY"]
	if registry == "" {
		t.Fatal("configmap UNBOUNDED_IMAGE_REGISTRY is empty")
	}

	// ghcr (and OCI generally) reject uppercase repository names, so the resolved
	// registry must be lowercase. This is cheap insurance against a release job
	// feeding the raw github.repository_owner (e.g. "Azure") without lowercasing.
	if registry != strings.ToLower(registry) {
		t.Fatalf("configmap UNBOUNDED_IMAGE_REGISTRY %q is not lowercase; OCI repository names must be lowercase", registry)
	}

	// The ConfigMap must carry the image-registry schema marker so kubectl
	// unbounded install recognizes a directly-applied config as already using the
	// full-prefix semantics and never migrates it.
	if got := cm.Metadata.Annotations["unbounded-cloud.io/image-registry-schema"]; got != "2" {
		t.Fatalf("configmap image-registry-schema annotation = %q, want \"2\"", got)
	}

	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `yaml:"name"`
						Image string `yaml:"image"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	readYAML(t, filepath.Join(outputDir, "04-deployment.yaml"), &deploy)

	var image string

	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "controller" {
			image = c.Image

			break
		}
	}

	if image == "" {
		t.Fatal("controller container image not found")
	}

	// The operator appends "/<component>:<tag>" to the registry, so its own image
	// must share that exact registry prefix or it would deploy components from a
	// different registry than itself.
	want := registry + "/unbounded-operator:" + tag
	if image != want {
		t.Fatalf("operator image %q does not match component registry %q (want %q)", image, registry, want)
	}
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("reached filesystem root without finding go.mod (started at %s)", filepath.Dir(file))
		}

		dir = parent
	}
}
