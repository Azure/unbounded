// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/noderoute"
)

func TestSharedCredentialFromStdin(t *testing.T) {
	options := &registryAddOptions{
		root:          &rootOptions{namespace: "gantry-system"},
		username:      "reader",
		passwordStdin: true,
	}
	command := &cobra.Command{}
	command.SetIn(strings.NewReader("secret-value\n"))

	username, password, err := options.sharedCredential(t.Context(), command, fake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("sharedCredential: %v", err)
	}

	if username != "reader" || password != "secret-value" {
		t.Fatalf("credential = %q/%q", username, password)
	}
}

func TestBaseInstallExcludesNodeConfigManifest(t *testing.T) {
	if baseInstallManifest(nodeConfigManifestName) {
		t.Fatal("base install included the host-routing DaemonSet")
	}

	for _, name := range []string{"00-rbac.yaml.tmpl", "01-config.yaml.tmpl", "02-agent.yaml.tmpl"} {
		if !baseInstallManifest(name) {
			t.Fatalf("base install excluded %s", name)
		}
	}
}

func TestNodeConfigRequired(t *testing.T) {
	for _, test := range []struct {
		name       string
		exists     bool
		routeCount int
		want       bool
	}{
		{name: "fresh inert install", want: false},
		{name: "existing daemonset", exists: true, want: true},
		{name: "missing daemonset with preserved routes", routeCount: 1, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nodeConfigRequired(test.exists, test.routeCount); got != test.want {
				t.Fatalf("nodeConfigRequired(%t, %d) = %t, want %t", test.exists, test.routeCount, got, test.want)
			}
		})
	}
}

func TestSharedCredentialFromArbitrarilyNamedSecret(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-pull-auth", Namespace: "workloads"},
		Data: map[string][]byte{
			"login": []byte("team-reader"),
			"token": []byte("team-secret"),
		},
	})
	options := &registryAddOptions{
		root:            &rootOptions{namespace: "gantry-system"},
		fromSecret:      "team-a-pull-auth",
		secretNamespace: "workloads",
		usernameKey:     "login",
		passwordKey:     "token",
	}

	username, password, err := options.sharedCredential(t.Context(), &cobra.Command{}, client)
	if err != nil {
		t.Fatalf("sharedCredential: %v", err)
	}

	if username != "team-reader" || password != "team-secret" {
		t.Fatalf("credential = %q/%q", username, password)
	}
}

func TestPullTestPodPreservesSecretNamesAndServiceAccount(t *testing.T) {
	pod := pullTestPod(
		"team-a",
		"node-a",
		"registry.example.com/team/image:v1",
		"build-runner",
		[]corev1.LocalObjectReference{{Name: "first-secret"}, {Name: "different-name"}},
	)

	if pod.Namespace != "team-a" || pod.Spec.NodeName != "node-a" || pod.Spec.ServiceAccountName != "build-runner" {
		t.Fatalf("unexpected pull Pod placement: %#v", pod.Spec)
	}

	if got := pod.Spec.ImagePullSecrets; len(got) != 2 || got[0].Name != "first-secret" || got[1].Name != "different-name" {
		t.Fatalf("imagePullSecrets = %#v", got)
	}
}

func TestRegistryStoreEncodeKeepsCredentialOutOfConfigMaps(t *testing.T) {
	config := gantryconfig.NewDefault()
	config.AllowNoUpstreamRegistries = true
	secret := newOwnedSecret("gantry-system")
	secret.Data["registry-key"] = []byte("reader:highly-secret")
	store := &registryStore{
		agentConfig: config,
		auth:        registryAuthMetadata{},
		routes:      noderoute.Config{},
		configData:  map[string]string{},
		routesData:  map[string]string{},
		secret:      secret,
	}
	store.upsertRegistry(
		gantryconfig.UpstreamRegistry{
			Name:            "registry.example.com",
			Endpoint:        "https://registry.example.com",
			AuthMode:        gantryconfig.UpstreamAuthShared,
			CredentialsPath: "/etc/gantry/registry/registry-key",
		},
		registryAuthRecord{Mode: authBasic, CredentialKey: "registry-key"},
		noderoute.Registry{Host: "registry.example.com", Server: "https://registry.example.com"},
	)

	if err := store.encode(); err != nil {
		t.Fatalf("encode: %v", err)
	}

	for key, value := range store.configData {
		if strings.Contains(value, "highly-secret") || strings.Contains(value, "reader:") {
			t.Fatalf("credential leaked into ConfigMap key %s", key)
		}
	}

	loaded := gantryconfig.NewDefault()
	if err := loaded.LoadYAML(strings.NewReader(store.configData["config.yaml"])); err != nil {
		t.Fatalf("reload generated config: %v", err)
	}

	loaded.AllowNoUpstreamRegistries = true
	if err := loaded.Validate(); err != nil {
		t.Fatalf("validate generated config: %v", err)
	}

	if got := loaded.UpstreamRegistries[0].AuthMode; got != gantryconfig.UpstreamAuthShared {
		t.Fatalf("generated auth mode = %q", got)
	}
}

func TestRegistryStorePromotesReplacementManagementOnce(t *testing.T) {
	store := &registryStore{routes: noderoute.Config{Registries: []noderoute.Registry{{
		Host: "registry.example.com", Server: "https://registry.example.com",
	}}}}

	if !store.manageRouteReplacements("registry.example.com") {
		t.Fatal("first promotion reported no change")
	}

	if !store.routes.Registries[0].ManageReplacements {
		t.Fatal("route was not promoted")
	}

	if store.manageRouteReplacements("registry.example.com") {
		t.Fatal("second promotion reported a change")
	}
}

func TestStagedSecretPreservesExistingKeys(t *testing.T) {
	existing := newOwnedSecret("gantry-system")
	existing.Data["old-key"] = []byte("old")
	client := fake.NewSimpleClientset(existing)
	desired := newOwnedSecret("gantry-system")
	desired.Data["new-key"] = []byte("new")

	staged, err := stagedSecret(t.Context(), client, "gantry-system", desired)
	if err != nil {
		t.Fatalf("stagedSecret: %v", err)
	}

	if string(staged.Data["old-key"]) != "old" || string(staged.Data["new-key"]) != "new" {
		t.Fatalf("staged data = %#v", staged.Data)
	}
}

func TestMetricSum(t *testing.T) {
	metrics := []byte(`# HELP p2p_cache_hit_total hits
p2p_cache_hit_total 2
p2p_cache_hit_total{source="local"} 3
p2p_cache_miss_total 4
unrelated 100
`)
	if got := metricSum(metrics, "p2p_cache_hit_total"); got != 5 {
		t.Fatalf("metricSum = %v, want 5", got)
	}
}

func TestRegistryFromImage(t *testing.T) {
	if got, err := registryFromImage("MyACR.azurecr.io/team/image:v1"); err != nil || got != "myacr.azurecr.io" {
		t.Fatalf("registryFromImage = %q, %v", got, err)
	}

	if _, err := registryFromImage("alpine:latest"); err == nil {
		t.Fatal("registryFromImage accepted an implicit registry")
	}
}

func TestDaemonSetReady(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			DesiredNumberScheduled: 3,
			UpdatedNumberScheduled: 3,
			NumberReady:            3,
		},
	}
	if !daemonSetReady(daemonSet) {
		t.Fatal("complete DaemonSet was not ready")
	}

	daemonSet.Status.NumberReady = 2
	if daemonSetReady(daemonSet) {
		t.Fatal("partial DaemonSet was ready")
	}
}
