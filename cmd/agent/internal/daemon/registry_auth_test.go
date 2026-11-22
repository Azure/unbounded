// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
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

	require.Equal(t, registryAuthBaseBackoff, registryAuthBackoff(0))
	require.Equal(t, registryAuthBaseBackoff*2, registryAuthBackoff(1))
	require.Equal(t, registryAuthBaseBackoff*4, registryAuthBackoff(2))
	require.Equal(t, registryAuthMaxBackoff, registryAuthBackoff(20))
}

func TestLoadSaveRegistryAuthBudget(t *testing.T) {
	orig := registryAuthBudgetDir

	registryAuthBudgetDir = t.TempDir()
	defer func() { registryAuthBudgetDir = orig }()

	slot := "kube1"

	b, err := loadRegistryAuthBudget(slot)
	require.NoError(t, err)
	require.Equal(t, 0, b.Attempts)

	b.Attempts = 2
	b.PolicyHash = "abc"
	b.LastAttempt = time.Now().Truncate(time.Second)
	require.NoError(t, saveRegistryAuthBudget(slot, b))

	reloaded, err := loadRegistryAuthBudget(slot)
	require.NoError(t, err)
	require.Equal(t, 2, reloaded.Attempts)
	require.Equal(t, "abc", reloaded.PolicyHash)
}

func TestPendingWorkRecordRoundTrip(t *testing.T) {
	t.Parallel()

	orig := registryAuthBudgetDir

	registryAuthBudgetDir = t.TempDir()
	defer func() { registryAuthBudgetDir = orig }()

	r := &registryAuthReconciler{}
	slot := "kube1"

	pending, err := r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "", pending.PolicyHash)
	require.False(t, pending.Work)

	require.NoError(t, r.savePending(slot, "abc", true))
	pending, err = r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "abc", pending.PolicyHash)
	require.True(t, pending.Work)

	require.NoError(t, r.clearPendingWork(slot, "abc"))
	pending, err = r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "abc", pending.PolicyHash)
	require.False(t, pending.Work)

	path := registryAuthPendingPath(slot)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, err = r.readPending(slot)
	require.Error(t, err)
}

func TestRegistryAuthFailureOutcome(t *testing.T) {
	t.Parallel()

	reason, message, after := registryAuthFailureOutcome("", &registryAuthBudgetExhausted{budget: &registryAuthBudget{Attempts: registryAuthMaxAttempts}})
	require.Equal(t, "RestartBudgetExhausted", reason)
	require.Contains(t, message, "exhausted")
	require.Equal(t, time.Duration(0), after)

	reason, _, after = registryAuthFailureOutcome("", &registryAuthBackoffPending{wait: 5 * time.Minute})
	require.Equal(t, "RestartBackoff", reason)
	require.Equal(t, 5*time.Minute, after)

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

	changed, err = writeFileIfChanged(path, []byte("one"), 0o644)
	require.NoError(t, err)
	require.False(t, changed)

	changed, err = writeFileIfChanged(path, []byte("two"), 0o644)
	require.NoError(t, err)
	require.True(t, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("two"), data)
}

// TestApplyProviderToNodeConfig exercises the REAL apply path end to end:
// applyProviderToNodeConfig -> nodestart.RenderNodeConfigDropIn -> the file on
// disk under an isolation root. It uses isolation.NewDirRoot so the drop-in is
// written to a real temp directory, and it asserts on the rendered content (not
// a fake string callback) so a regression that writes an empty file, clobbers a
// customer provider, or fails to detect a change is caught.
func TestApplyProviderToNodeConfig(t *testing.T) {
	customerProvider := &goalstates.ImageCredentialProvider{
		ConfigPath: "/etc/customer/provider.json",
		BinDir:     "/opt/customer/providers",
	}

	tt := []struct {
		name       string
		policy     *registryauth.Config
		customer   *goalstates.ImageCredentialProvider
		wantChange bool
		wantErr    bool
		wantFlags  bool
		// wantCustomer asserts the rendered drop-in still carries the customer
		// provider (external mode with a customer config) and NOT the managed pair.
		wantCustomer bool
	}{
		{
			name:       "attach with no customer provider adds managed flags",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			customer:   nil,
			wantChange: true,
			wantFlags:  true,
		},
		{
			name:       "attach rejects a different customer provider",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			customer:   customerProvider,
			wantChange: false,
			wantErr:    true,
		},
		{
			name:       "external with no customer provider leaves none",
			policy:     nil,
			customer:   nil,
			wantChange: true,
			wantFlags:  false,
		},
		{
			name:         "external preserves a customer provider",
			policy:       nil,
			customer:     customerProvider,
			wantChange:   true,
			wantFlags:    false,
			wantCustomer: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			root, err := isolation.NewDirRoot("slot1", rootDir)
			require.NoError(t, err)

			gs := &goalstates.MachineGoalState{
				NodeStart: &goalstates.NodeStart{
					NodeName: "node1",
					Kubelet: goalstates.Kubelet{
						NodeIP:                  "10.0.0.5",
						ImageCredentialProvider: tc.customer,
					},
				},
			}

			// The drop-in is written under the node-stack root at the mode-correct
			// path (nspawn -> kubelet.service.d/20-node-config.conf).
			path := filepath.Join(rootDir, goalstates.SystemdSystemDir, "kubelet.service.d", "20-node-config.conf")

			changed, err := applyProviderToNodeConfig(root, isolation.ModeNSpawn, nil, gs, tc.policy)
			if tc.wantErr {
				require.Error(t, err)
				require.False(t, changed)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantChange, changed)

			data, readErr := os.ReadFile(path)
			if !tc.wantChange {
				// No write happened when nothing changed; the file need not exist yet.
				require.True(t, os.IsNotExist(readErr) || readErr != nil, "no change expected but file may or may not exist")

				return
			}

			require.NoError(t, readErr)

			if tc.wantFlags {
				require.Contains(t, string(data), "--image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath)
				require.Contains(t, string(data), "--image-credential-provider-bin-dir="+goalstates.RegistryAuthProviderBinDir)
			} else {
				require.NotContains(t, string(data), "--image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath)
				require.NotContains(t, string(data), "--image-credential-provider-bin-dir="+goalstates.RegistryAuthProviderBinDir)
			}

			if tc.wantCustomer {
				// The customer provider must survive; the managed pair must not appear.
				require.Contains(t, string(data), "--image-credential-provider-config="+customerProvider.ConfigPath)
				require.Contains(t, string(data), "--image-credential-provider-bin-dir="+customerProvider.BinDir)
			}

			// The non-provider node args are always preserved by the template.
			require.Contains(t, string(data), "--hostname-override=node1")
			require.Contains(t, string(data), "--node-ip=10.0.0.5")
		})
	}
}

// TestApplyProviderToNodeConfigRoundtrip drives the real apply path through a
// full attach -> detach round trip on one root, asserting the on-disk content at
// each step and that detach removes only the managed pair (leaving a customer
// provider, if present, intact).
func TestApplyProviderToNodeConfigRoundtrip(t *testing.T) {
	rootDir := t.TempDir()
	root, err := isolation.NewDirRoot("slot1", rootDir)
	require.NoError(t, err)

	gs := &goalstates.MachineGoalState{
		NodeStart: &goalstates.NodeStart{
			NodeName: "node1",
			Kubelet: goalstates.Kubelet{
				NodeIP: "10.0.0.5",
			},
		},
	}
	path := filepath.Join(rootDir, goalstates.SystemdSystemDir, "kubelet.service.d", "20-node-config.conf")

	// Step 1: attach. The managed pair appears.
	changed, err := applyProviderToNodeConfig(root, isolation.ModeNSpawn, nil, gs, &registryauth.Config{Mode: registryauth.IdentityBindingMode})
	require.NoError(t, err)
	require.True(t, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "--image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath)
	require.Contains(t, string(data), "--image-credential-provider-bin-dir="+goalstates.RegistryAuthProviderBinDir)

	// Step 2: detach with no customer provider. The managed pair is removed.
	changed, err = applyProviderToNodeConfig(root, isolation.ModeNSpawn, nil, gs, nil)
	require.NoError(t, err)
	require.True(t, changed)

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(data), "--image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath)
	require.NotContains(t, string(data), "--image-credential-provider-bin-dir="+goalstates.RegistryAuthProviderBinDir)

	// Step 3: attach again on a fresh goal state that carries a customer
	// provider -> must be rejected, and the on-disk content must be untouched.
	gs2 := &goalstates.MachineGoalState{
		NodeStart: &goalstates.NodeStart{
			NodeName: "node1",
			Kubelet: goalstates.Kubelet{
				NodeIP: "10.0.0.5",
				ImageCredentialProvider: &goalstates.ImageCredentialProvider{
					ConfigPath: "/etc/customer/provider.json",
					BinDir:     "/opt/customer/providers",
				},
			},
		},
	}
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	changed, err = applyProviderToNodeConfig(root, isolation.ModeNSpawn, nil, gs2, &registryauth.Config{Mode: registryauth.IdentityBindingMode})
	require.Error(t, err)
	require.False(t, changed)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "rejected attach must not modify the drop-in")
}

func TestEffectiveManagedFlags(t *testing.T) {
	t.Parallel()

	m0, m1 := registryAuthProviderFlags()[0], registryAuthProviderFlags()[1]

	tt := []struct {
		name string
		argv []string
		want bool
	}{
		{
			name: "managed flags effective",
			argv: []string{"kubelet", "--hostname-override=n1", m0, m1},
			want: true,
		},
		{
			name: "no provider flags",
			argv: []string{"kubelet", "--hostname-override=n1"},
			want: false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, registryAuthEffectiveManagedFlags(tc.argv))
		})
	}
}

// TestVerifyKubeletExternalSplitFormManagedFlags is the regression the review
// called out: the external-mode verifyKubelet check must NOT acknowledge a
// running kubelet whose argv carries the managed provider in SPLIT form
// (--flag value, two tokens), as produced by KUBELET_EXTRA_ARGS or an
// environment-sourced drop-in. A bare exact-inline-token match would miss the
// split form and falsely conclude the managed provider was removed. The check
// must detect the managed provider in effect from the split-form argv, whether
// both flags or a single remaining managed flag are present.
func TestVerifyKubeletExternalSplitFormManagedFlags(t *testing.T) {
	t.Parallel()

	cfg := goalstates.RegistryAuthCredentialConfigPath
	bin := goalstates.RegistryAuthProviderBinDir

	tt := []struct {
		name string
		argv []string
		// wantHasManaged is what registryAuthArgvHasManagedFlags (the external
		// check) must return. true == managed provider still in effect, so the
		// external acknowledgment must be rejected.
		wantHasManaged bool
	}{
		{
			name:           "split-form both managed flags in effect",
			argv:           []string{"kubelet", "--image-credential-provider-config", cfg, "--image-credential-provider-bin-dir", bin},
			wantHasManaged: true,
		},
		{
			name:           "split-form single remaining managed config flag",
			argv:           []string{"kubelet", "--image-credential-provider-config", cfg},
			wantHasManaged: true,
		},
		{
			name:           "split-form single remaining managed bin-dir flag",
			argv:           []string{"kubelet", "--image-credential-provider-bin-dir", bin},
			wantHasManaged: true,
		},
		{
			name:           "inline-form managed flags in effect",
			argv:           []string{"kubelet", "--image-credential-provider-config=" + cfg, "--image-credential-provider-bin-dir=" + bin},
			wantHasManaged: true,
		},
		{
			name:           "customer split-form provider is not the managed one",
			argv:           []string{"kubelet", "--image-credential-provider-config", "/etc/customer/provider.json", "--image-credential-provider-bin-dir", "/opt/customer/providers"},
			wantHasManaged: false,
		},
		{
			name:           "no provider flags at all",
			argv:           []string{"kubelet", "--hostname-override=n1", "--node-ip=10.0.0.5"},
			wantHasManaged: false,
		},
		{
			name:           "split-form repeated flag overridden to customer value",
			argv:           []string{"kubelet", "--image-credential-provider-config", cfg, "--image-credential-provider-config", "/etc/customer/provider.json"},
			wantHasManaged: false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantHasManaged, registryAuthArgvHasManagedFlags(tc.argv),
				"external-mode verifyKubelet must recognize the managed provider in this argv form")
		})
	}
}

// TestVerifyKubeletExternalSplitFormIsRegression exercises the full
// verifyKubelet method with a FakeBackend whose Exec returns a MainPID and a
// NUL-separated /proc/<pid>/cmdline carrying the managed provider flags in
// split form. This is the exact counterexample from the review: a kubelet
// running with KUBELET_EXTRA_ARGS supplying the managed provider in split form
// must NOT be acknowledged as external.
func TestVerifyKubeletExternalSplitFormIsRegression(t *testing.T) {
	t.Parallel()

	cfg := goalstates.RegistryAuthCredentialConfigPath
	bin := goalstates.RegistryAuthProviderBinDir
	argv := []string{
		"kubelet",
		"--image-credential-provider-config", cfg,
		"--image-credential-provider-bin-dir", bin,
	}

	// FakeBackend: first Exec call is systemctl show MainPID, second is
	// cat /proc/<pid>/cmdline.
	callCount := 0
	backend := &isolation.FakeBackend{
		FakeMode: isolation.ModeNSpawn,
		Dir:      t.TempDir(),
		ExecFunc: func(spec isolation.ExecSpec) (isolation.ExecResult, error) {
			callCount++
			switch {
			case len(spec.Args) > 0 && spec.Args[0] == "systemctl":
				return isolation.ExecResult{Stdout: "4242\n"}, nil
			case len(spec.Args) > 0 && spec.Args[0] == "cat":
				return isolation.ExecResult{Stdout: strings.Join(argv, "\x00")}, nil
			default:
				t.Fatalf("unexpected Exec: %v", spec.Args)
			}

			return isolation.ExecResult{}, nil
		},
	}

	r := &registryAuthReconciler{
		log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
		machineName: "test-machine",
		backends: ModeBackends{
			isolation.ModeNSpawn: func(log *slog.Logger, cfg *provision.AgentConfig) isolation.Backend {
				return backend
			},
		},
	}

	active := &ActiveMachine{
		Name: "test-machine",
		Mode: isolation.ModeNSpawn,
	}

	// External policy: active=false means we want to confirm no managed flags.
	desired := registryAuthActivePolicy{active: false}

	ready, err := r.verifyKubelet(context.Background(), active, desired)
	require.NoError(t, err)
	require.False(t, ready, "verifyKubelet must reject external acknowledgment when split-form managed flags are in the running kubelet argv")
}
