//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/gantry/config"
)

// This is offline: it validates the exact runtime fixtures without a cluster.
func TestAuthRegistryFixturesUseRequesterCredentials(t *testing.T) {
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(authRegistryConfigManifest()), &cm); err != nil {
		t.Fatal(err)
	}

	cfg := config.NewDefault()
	if err := cfg.LoadYAML(strings.NewReader(cm.Data["config.yaml"])); err != nil {
		t.Fatal(err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	var secret corev1.Secret
	if err := yaml.Unmarshal([]byte(authRegistrySecretManifest()), &secret); err != nil {
		t.Fatal(err)
	}

	if secret.Namespace != "default" || secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("wrong credential owner/type: %s/%s %s", secret.Namespace, secret.Name, secret.Type)
	}

	var docker struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &docker); err != nil {
		t.Fatal(err)
	}

	if docker.Auths[authRegistryRefShort].Auth == "" {
		t.Fatal("registry credential missing")
	}

	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(authRegistryPodManifest("pull", "worker", authRegistryRefShort+"/agnhost:fixture")), &pod); err != nil {
		t.Fatal(err)
	}

	if pod.Namespace != secret.Namespace || len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != secret.Name {
		t.Fatal("workload does not reference its credential")
	}

	for _, removed := range []string{"credentials_path", "storage_mode", "members_label_selector"} {
		if strings.Contains(cm.Data["config.yaml"], removed) {
			t.Fatalf("removed setting %s emitted", removed)
		}
	}
}
