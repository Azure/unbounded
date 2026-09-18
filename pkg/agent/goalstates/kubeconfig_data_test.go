// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"bytes"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/config"
)

func TestResolveKubeletPropagatesKubeconfigData(t *testing.T) {
	t.Parallel()

	data := []byte(`apiVersion: v1
kind: Config
current-context: current
clusters:
- name: cluster
  cluster:
    server: https://api.example.com
users:
- name: user
  user:
    token: embedded
contexts:
- name: current
  context:
    cluster: cluster
    user: user
`)
	cfg := &config.AgentConfig{
		Kubelet: config.AgentKubeletConfig{
			KubeconfigData: data,
		},
	}

	got, err := resolveKubelet(cfg)
	if err != nil {
		t.Fatalf("resolveKubelet() error = %v", err)
	}
	if !bytes.Equal(got.KubeconfigData, data) {
		t.Fatalf("KubeconfigData = %q, want %q", got.KubeconfigData, data)
	}

	data[0] = 'X'
	if got.KubeconfigData[0] == data[0] {
		t.Fatal("resolved KubeconfigData aliases agent configuration")
	}
}
