// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundedoperator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
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
		"MetalmanImage":     "metalman:test",
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

	// The Deployment pod template carries the matching hash annotation.
	var deploy struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Containers []struct {
						Name         string `yaml:"name"`
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

	sum := sha256.Sum256([]byte(endpoint))
	want := hex.EncodeToString(sum[:])

	if got := deploy.Spec.Template.Metadata.Annotations["unbounded-cloud.io/operator-config-hash"]; got != want {
		t.Fatalf("deployment operator-config-hash annotation = %q, want %q (sha256 of endpoint)", got, want)
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name != "controller" || container.StartupProbe == nil {
			continue
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

func TestOperatorRBACIncludesCachedReadKinds(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	if err := render.Render(filepath.Join(repoRoot(t), "deploy", "unbounded-operator"), outputDir, map[string]string{
		"Namespace":     "unbounded-system",
		"OperatorImage": "operator:test",
		"MetalmanImage": "metalman:test",
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
