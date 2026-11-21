// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestDesiredRegistryAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		spec         *v1alpha3.RegistryAuthPolicy
		wantActive   bool
		wantRegistry string
	}{
		{
			name:       "absent policy is external",
			spec:       nil,
			wantActive: false,
		},
		{
			name:       "external mode is external",
			spec:       &v1alpha3.RegistryAuthPolicy{Mode: registryauth.ExternalMode},
			wantActive: false,
		},
		{
			name: "identity binding is active",
			spec: &v1alpha3.RegistryAuthPolicy{
				Mode: registryauth.IdentityBindingMode, Registry: "myacr.azurecr.io", ClientID: "a", TenantID: "b",
				SNIName: "c", ProxyAddress: "d", Audience: "e", CACert: "f",
			},
			wantActive:   true,
			wantRegistry: "myacr.azurecr.io",
		},
		{
			name:       "unknown mode is external",
			spec:       &v1alpha3.RegistryAuthPolicy{Mode: "bogus"},
			wantActive: false,
		},
	}

	for i := range tests {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := desiredRegistryAuth(&v1alpha3.Machine{Spec: v1alpha3.MachineSpec{RegistryAuth: tc.spec}})
			require.Equal(t, tc.wantActive, got.active)
			if tc.wantActive {
				require.Equal(t, tc.wantRegistry, got.policy.Registry)
				require.NotEmpty(t, got.policyHash)
			}
		})
	}
}

func TestRegistryAuthPolicyHashStable(t *testing.T) {
	t.Parallel()

	p1 := registryauth.Config{Mode: registryauth.IdentityBindingMode, Registry: "r", ClientID: "c", TenantID: "t"}
	p2 := registryauth.Config{Mode: registryauth.IdentityBindingMode, Registry: "r", ClientID: "c", TenantID: "t"}
	p3 := registryauth.Config{Mode: registryauth.IdentityBindingMode, Registry: "other", ClientID: "c", TenantID: "t"}

	require.Equal(t, registryAuthPolicyHash(p1), registryAuthPolicyHash(p2))
	require.NotEqual(t, registryAuthPolicyHash(p1), registryAuthPolicyHash(p3))
}

func TestRegistryAuthBackoff(t *testing.T) {
	t.Parallel()

	// 0 attempts (about to make the first): base backoff.
	require.Equal(t, registryAuthBaseBackoff, registryAuthBackoff(0))
	// After 1 attempt: 2x base.
	require.Equal(t, registryAuthBaseBackoff*2, registryAuthBackoff(1))
	// After 2 attempts: 4x base.
	require.Equal(t, registryAuthBaseBackoff*4, registryAuthBackoff(2))
	// After many attempts: capped at the max.
	require.Equal(t, registryAuthMaxBackoff, registryAuthBackoff(20))
}

func TestLoadSaveRegistryAuthBudget(t *testing.T) {
	orig := registryAuthBudgetDir
	registryAuthBudgetDir = t.TempDir()
	defer func() { registryAuthBudgetDir = orig }()

	slot := "kube1"

	// Fresh budget when the file is missing.
	b, err := loadRegistryAuthBudget(slot)
	require.NoError(t, err)
	require.Equal(t, 0, b.Attempts)

	// Save and reload.
	b.Attempts = 2
	b.PolicyHash = "abc"
	b.LastAttempt = time.Now().Truncate(time.Second)
	require.NoError(t, saveRegistryAuthBudget(slot, b))

	reloaded, err := loadRegistryAuthBudget(slot)
	require.NoError(t, err)
	require.Equal(t, 2, reloaded.Attempts)
	require.Equal(t, "abc", reloaded.PolicyHash)
}

func TestRegistryAuthFailureOutcome(t *testing.T) {
	t.Parallel()

	// Budget exhausted reports the terminal reason with no requeue backoff.
	reason, message, after := registryAuthFailureOutcome("", &registryAuthBudgetExhausted{budget: &registryAuthBudget{Attempts: registryAuthMaxAttempts}})
	require.Equal(t, "RestartBudgetExhausted", reason)
	require.Contains(t, message, "exhausted")
	require.Equal(t, time.Duration(0), after)

	// Backoff pending reports a wait.
	reason, _, after = registryAuthFailureOutcome("", &registryAuthBackoffPending{wait: 5 * time.Minute})
	require.Equal(t, "RestartBackoff", reason)
	require.Equal(t, 5*time.Minute, after)

	// A generic failure reports a retryable reason.
	reason, _, after = registryAuthFailureOutcome("", os.ErrInvalid)
	require.Equal(t, "RestartFailed", reason)
	require.Equal(t, registryAuthRequeue, after)
}

func TestWriteFileIfChanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out")

	changed, err := writeFileIfChanged(path, []byte("one"), 0o644)
	require.NoError(t, err)
	require.True(t, changed)

	// Same content: no change.
	changed, err = writeFileIfChanged(path, []byte("one"), 0o644)
	require.NoError(t, err)
	require.False(t, changed)

	// Different content: change.
	changed, err = writeFileIfChanged(path, []byte("two"), 0o644)
	require.NoError(t, err)
	require.True(t, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("two"), data)
}

func TestRegistryAuthDropInContent(t *testing.T) {
	t.Parallel()

	execStart := "/usr/local/bin/kubelet --config=/var/lib/kubelet/config.yaml $KUBELET_NODE_CONFIG_ARGS"
	data := registryAuthDropInContent(execStart)

	require.Contains(t, data, registryAuthManagedMarker)
	require.Contains(t, data, "ExecStart="+execStart+" --image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath)
	require.Contains(t, data, "--image-credential-provider-bin-dir="+goalstates.RegistryAuthProviderBinDir)
}
