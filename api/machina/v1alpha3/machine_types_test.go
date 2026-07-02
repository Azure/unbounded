// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestKubernetesSpecOmitsUnsetBootstrapTokenRef(t *testing.T) {
	spec := KubernetesSpec{
		NodeLabels: map[string]string{"example.com/test": "true"},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal KubernetesSpec: %v", err)
	}

	if !bytes.Contains(data, []byte(`"nodeLabels"`)) {
		t.Fatalf("marshaled KubernetesSpec = %s, want nodeLabels", data)
	}

	if bytes.Contains(data, []byte(`"bootstrapTokenRef"`)) {
		t.Fatalf("marshaled KubernetesSpec = %s, want bootstrapTokenRef omitted", data)
	}
}

func TestAgentSpecAdditionalHostDevicesJSON(t *testing.T) {
	spec := AgentSpec{
		AdditionalHostDevices: []string{"/dev/uinput"},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal AgentSpec: %v", err)
	}

	if !bytes.Contains(data, []byte(`"additionalHostDevices":["/dev/uinput"]`)) {
		t.Fatalf("marshaled AgentSpec = %s, want additionalHostDevices", data)
	}
}
