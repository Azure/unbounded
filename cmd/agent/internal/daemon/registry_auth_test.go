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

func TestPendingWorkRecordRoundTrip(t *testing.T) {
	t.Parallel()

	orig := registryAuthBudgetDir

	registryAuthBudgetDir = t.TempDir()
	defer func() { registryAuthBudgetDir = orig }()

	r := &registryAuthReconciler{}
	slot := "kube1"

	// A fresh slot has no record: work is not owed, no revision tied.
	pending, err := r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "", pending.PolicyHash)
	require.False(t, pending.Work)

	// Persisting the intent (work owed) for a revision reads back as owed.
	require.NoError(t, r.savePending(slot, "abc", true))
	pending, err = r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "abc", pending.PolicyHash)
	require.True(t, pending.Work, "a persisted intent must read back as work owed")

	// clearPendingWork records the revision as verified (work not owed) without
	// removing the file, so the next pass can tell the revision is verified
	// rather than treating the empty hash as a fresh revision change.
	require.NoError(t, r.clearPendingWork(slot, "abc"))
	pending, err = r.readPending(slot)
	require.NoError(t, err)
	require.Equal(t, "abc", pending.PolicyHash, "clearPendingWork must retain the verified revision")
	require.False(t, pending.Work, "a verified revision owes no restart")

	// A corrupt pending file surfaces as an error, not a free restart.
	path := registryAuthPendingPath(slot)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, err = r.readPending(slot)
	require.Error(t, err, "a corrupt pending record must not grant a free restart")
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

// managedFlags returns the two managed provider tokens, matching
// registryAuthProviderFlags, used to build and check drop-in contents.
func managedFlags() []string {
	return registryAuthProviderFlags()
}

// applyDropIn points the drop-in path override at a fresh temp file with the
// given contents and returns the path. The subtests are NOT parallel because
// the path override is a package global; concurrent subtests would race on it.
func applyDropIn(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "kubelet.service.d", "20-node-config.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	orig := registryAuthNodeConfigDropInPath
	registryAuthNodeConfigDropInPath = func(_ isolation.NodeRoot, _ isolation.Mode) string { return path }

	t.Cleanup(func() { registryAuthNodeConfigDropInPath = orig })

	return path
}

// generatedDropIn is the exact format the nodestart phase renders for a node
// with hostname-override and node-ip and no provider flags yet.
const generatedDropIn = "# Managed by unbounded-agent. Do not edit directly.\n" +
	"[Service]\n" +
	"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --node-ip=10.0.0.5\"\n"

func TestNodeConfigArgsValue(t *testing.T) {
	t.Parallel()

	tt := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{
			name: "generated quoted form",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --node-ip=10.0.0.5\"\n",
			want: "--hostname-override=node1 --node-ip=10.0.0.5",
		},
		{
			name: "generated quoted form with provider flags",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --image-credential-provider-config=/x --image-credential-provider-bin-dir=/y\"\n",
			want: "--hostname-override=node1 --image-credential-provider-config=/x --image-credential-provider-bin-dir=/y",
		},
		{
			name: "bare unquoted form",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1\n",
			want: "--hostname-override=node1",
		},
		{
			name:    "no matching line",
			data:    "[Service]\nEnvironment=FOO=bar\n",
			wantErr: true,
		},
		{
			name: "value-quoted form unquotes the value",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--hostname-override=node --node-ip=10.0.0.5\"\n",
			want: "--hostname-override=node --node-ip=10.0.0.5",
		},
		{
			name: "last of several assignments is the effective value",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=base\"\n" +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=base --node-ip=10.0.0.5\"\n",
			want: "--hostname-override=base --node-ip=10.0.0.5",
		},
		{
			name: "customer provider after base is the effective value",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=base\"\n" +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=base --image-credential-provider-config=/customer/cfg.json --image-credential-provider-bin-dir=/customer/bin\"\n",
			want: "--hostname-override=base --image-credential-provider-config=/customer/cfg.json --image-credential-provider-bin-dir=/customer/bin",
		},
		{
			name: "split-form provider is parsed as name and value",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--hostname-override=base --image-credential-provider-config /customer/cfg.json --image-credential-provider-bin-dir /customer/bin\"\n",
			want: "--hostname-override=base --image-credential-provider-config /customer/cfg.json --image-credential-provider-bin-dir /customer/bin",
		},
		{
			// The whole-assignment-quoted form must unescape the value's
			// backslashes, not return them doubled: systemd double-quoted values
			// use C-style escapes, so \\\\ is a literal \\.
			name: "whole-assignment-quoted backslash is unescaped",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--foo=/tmp/a\\\\b\"\n",
			want: "--foo=/tmp/a\\b",
		},
		{
			// A single Environment= directive may carry several separately
			// quoted items; only the KUBELET item's value is returned, the
			// sibling item is not folded into the kubelet args.
			name: "sibling item is not folded into the kubelet args",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node\" \"UNRELATED=keep\"\n",
			want: "--hostname-override=node",
		},
		{
			// A repeated assignment to the same variable within ONE Environment=
			// directive: systemd uses the LAST value.
			name: "repeated assignment in one directive uses last value",
			data: "Environment=\"KUBELET_NODE_CONFIG_ARGS=--first\" \"KUBELET_NODE_CONFIG_ARGS=--last\"\n",
			want: "--last",
		},
		{
			// A bare Environment= (empty value) resets the environment: a
			// KUBELET_NODE_CONFIG_ARGS assigned before the reset is no longer
			// defined.
			name:    "empty Environment= reset clears prior assignment",
			data:    "Environment=\"KUBELET_NODE_CONFIG_ARGS=--base\"\nEnvironment=\n",
			wantErr: true,
		},
		{
			// A hex escape \x31 decodes to '1': node\x31 becomes node1.
			name: "hex escape is decoded",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--hostname-override=node\\x31\"\n",
			want: "--hostname-override=node1",
		},
		{
			// An octal escape \061 decodes to '1'.
			name: "octal escape is decoded",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--hostname-override=node\\061\"\n",
			want: "--hostname-override=node1",
		},
		{
			// A unicode escape \u0031 decodes to '1'.
			name: "unicode BMP escape is decoded",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--hostname-override=node\\u0031\"\n",
			want: "--hostname-override=node1",
		},
		{
			// The \s escape decodes to a space.
			name: "space escape is decoded",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--foo=a\\sb\"\n",
			want: "--foo=a b",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			value, err := nodeConfigArgsValue(tc.data)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
}

// TestNodeConfigArgsRenderedRoundTrip verifies the Environment codec is
// systemd-correct in both directions: decoding a quoted value and re-rendering
// it must write back a value systemd reads as the same string (a backslash is
// doubled on the way out), and a sibling item on the same Environment= directive
// must be preserved byte for byte.
func TestNodeConfigArgsRenderedRoundTrip(t *testing.T) {
	tt := []struct {
		name string
		line string
		args string
		want string
		keep string // a substring of a sibling item that must survive the rewrite
	}{
		{
			// A backslash decoded from a value-quoted form must be re-escaped
			// (doubled) so the round trip is stable.
			name: "backslash round-trips doubled",
			line: `Environment=KUBELET_NODE_CONFIG_ARGS="--foo=/tmp/a\\b"`,
			args: "--foo=/tmp/a\\b",
			want: `Environment=KUBELET_NODE_CONFIG_ARGS="--foo=/tmp/a\\b"`,
		},
		{
			// A sibling item on the same directive is preserved; only the
			// KUBELET item is re-encoded. The re-encode uses value-quote form
			// (KUBELET_NODE_CONFIG_ARGS="..."), so the expected output has the
			// value quoted, not the whole assignment.
			name: "sibling item preserved",
			line: `Environment="KUBELET_NODE_CONFIG_ARGS=--hostname-override=node" "UNRELATED=keep"`,
			args: "--hostname-override=node --extra=1",
			want: `Environment=KUBELET_NODE_CONFIG_ARGS="--hostname-override=node --extra=1" "UNRELATED=keep"`,
			keep: "UNRELATED=keep",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			rendered := rewriteKubeletItem(tc.line, tc.args)
			require.Equal(t, tc.want, rendered)

			// The re-rendered line must decode back to the same args.
			got, err := nodeConfigArgsValue(rendered + "\n")
			require.NoError(t, err)
			require.Equal(t, tc.args, got)

			if tc.keep != "" {
				require.Contains(t, rendered, tc.keep)
			}
		})
	}
}

// TestIndentedEnvironmentDirectiveIsRewritten verifies that an indented
// Environment= directive (inside a [Service] stanza) is recognized by both the
// getter and the renderer: attach adds the provider flags to the indented line,
// and detach removes them. The indentation is preserved.
func TestIndentedEnvironmentDirectiveIsRewritten(t *testing.T) {
	// A drop-in with an indented Environment= directive.
	indentedBase := "# Managed by unbounded-agent. Do not edit directly.\n" +
		"[Service]\n" +
		"  Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1\"\n"

	path := applyDropIn(t, indentedBase)

	// Attach: the managed flags must be added to the indented line.
	changed, err := applyProviderToNodeConfig(nil, isolation.ModeNone, &registryauth.Config{Mode: registryauth.IdentityBindingMode})
	require.NoError(t, err)
	require.True(t, changed, "attach must modify the indented directive")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	value, err := nodeConfigArgsValue(string(data))
	require.NoError(t, err)
	require.Contains(t, value, managedFlags()[0], "the managed config flag must be present after attach")
	require.Contains(t, value, managedFlags()[1], "the managed bin-dir flag must be present after attach")
	// The indentation must be preserved.
	require.Contains(t, string(data), "\n  Environment=")

	// Detach: the managed flags must be removed from the indented line.
	changed, err = applyProviderToNodeConfig(nil, isolation.ModeNone, nil)
	require.NoError(t, err)
	require.True(t, changed, "detach must modify the indented directive")

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	value, err = nodeConfigArgsValue(string(data))
	require.NoError(t, err)
	require.NotContains(t, value, managedFlags()[0], "the managed config flag must be absent after detach")
	require.NotContains(t, value, managedFlags()[1], "the managed bin-dir flag must be absent after detach")
}

// TestBareAndSingleQuotedEscapesDecode verifies that C-style escape sequences
// are decoded in bare (unquoted) and single-quoted items, not only in
// double-quoted items, matching systemd's extract_first_word behavior.
func TestBareAndSingleQuotedEscapesDecode(t *testing.T) {
	tt := []struct {
		name string
		data string
		want string
	}{
		{
			name: "bare item hex escape decodes",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=--hostname-override=node\\x31\n",
			want: "--hostname-override=node1",
		},
		{
			name: "single-quoted item hex escape decodes",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS='--hostname-override=node\\x31'\n",
			want: "--hostname-override=node1",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			value, err := nodeConfigArgsValue(tc.data)
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
}

// TestHexOctalByteEscapesProducesBytes verifies that hex and octal escapes
// produce raw bytes (not code points), so \xc3\xa9 produces the two bytes
// 0xC3 0xA9 (é in UTF-8), not U+00C3 + U+00A9.
func TestHexOctalByteEscapesProducesBytes(t *testing.T) {
	tt := []struct {
		name string
		data string
		want string
	}{
		{
			name: "hex byte escape produces UTF-8 bytes for e-acute",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--root-dir=/data/\\xc3\\xa9\"\n",
			want: "--root-dir=/data/é",
		},
		{
			name: "octal byte escape produces UTF-8 bytes for e-acute",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--root-dir=/data/\\303\\251\"\n",
			want: "--root-dir=/data/é",
		},
		{
			name: "unicode code point escape produces e-acute",
			data: "Environment=KUBELET_NODE_CONFIG_ARGS=\"--root-dir=/data/\\u00e9\"\n",
			want: "--root-dir=/data/é",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			value, err := nodeConfigArgsValue(tc.data)
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
}

func TestApplyProviderToNodeConfig(t *testing.T) {
	// Sequential on purpose: the drop-in path override is a package global and
	// concurrent subtests would race on it (the -race reproducer from review).
	tt := []struct {
		name       string
		base       string
		policy     *registryauth.Config
		wantChange bool
		wantErr    bool
		wantValue  string
	}{
		{
			name:       "adds managed flags and preserves other args",
			base:       generatedDropIn,
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: true,
			wantValue:  "--hostname-override=node1 --node-ip=10.0.0.5 " + managedFlags()[0] + " " + managedFlags()[1],
		},
		{
			name:       "no managed flags and no policy is unchanged",
			base:       generatedDropIn,
			policy:     nil,
			wantChange: false,
			wantValue:  "--hostname-override=node1 --node-ip=10.0.0.5",
		},
		{
			name: "detach removes managed flags preserving other args",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 " + managedFlags()[0] + " " + managedFlags()[1] + "\"\n",
			policy:     nil,
			wantChange: true,
			wantValue:  "--hostname-override=node1",
		},
		{
			name: "partial pair config only is completed with bin-dir",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 " + managedFlags()[0] + "\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: true,
			wantValue:  "--hostname-override=node1 " + managedFlags()[0] + " " + managedFlags()[1],
		},
		{
			// The existing bin-dir token stays in place; the missing config
			// flag is appended after it, so the completed order is bin-dir then
			// config (the ordering of the two managed flags is semantically
			// irrelevant, but the existing token is not moved).
			name: "partial pair bin-dir only is completed with config",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 " + managedFlags()[1] + "\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: true,
			wantValue:  "--hostname-override=node1 " + managedFlags()[1] + " " + managedFlags()[0],
		},
		{
			// A managed policy cannot be applied over an existing external
			// provider: addManaged refuses, so applyProviderToNodeConfig returns
			// an error and leaves the drop-in unchanged.
			name: "conflicting external provider is refused",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --image-credential-provider-config=/customer/cfg.json --image-credential-provider-bin-dir=/customer/bin\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: false,
			wantErr:    true,
			wantValue:  "--hostname-override=node1 --image-credential-provider-config=/customer/cfg.json --image-credential-provider-bin-dir=/customer/bin",
		},
		{
			name: "single conflicting flag is refused",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --image-credential-provider-config=/customer/cfg.json\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: false,
			wantErr:    true,
			wantValue:  "--hostname-override=node1 --image-credential-provider-config=/customer/cfg.json",
		},
		{
			// Finding 3: a SPLIT-FORM external provider (flag and value as
			// separate tokens) in the effective assignment is a conflict, not
			// "absent": the managed provider must not silently override it.
			name: "split-form external provider in effective assignment is refused",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --image-credential-provider-config /customer/cfg.json --image-credential-provider-bin-dir /customer/bin\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: false,
			wantErr:    true,
			wantValue:  "--hostname-override=node1 --image-credential-provider-config /customer/cfg.json --image-credential-provider-bin-dir /customer/bin",
		},
		{
			// Finding 3: a split-form external provider set via a LATER (effective)
			// assignment after a base assignment is still a conflict; the base
			// assignment alone must not be read.
			name: "split-form external provider in a later assignment is refused",
			base: generatedDropIn +
				"Environment=\"KUBELET_NODE_CONFIG_ARGS=--hostname-override=node1 --image-credential-provider-config /customer/cfg.json\"\n",
			policy:     &registryauth.Config{Mode: registryauth.IdentityBindingMode},
			wantChange: false,
			wantErr:    true,
			wantValue:  "--hostname-override=node1 --image-credential-provider-config /customer/cfg.json",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			path := applyDropIn(t, tc.base)

			changed, err := applyProviderToNodeConfig(nil, isolation.ModeNone, tc.policy)
			if tc.wantErr {
				// A conflict is refused: the apply returns an error and leaves
				// the drop-in unchanged.
				require.Error(t, err)
				require.False(t, changed)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantChange, changed)
			}

			data, err := os.ReadFile(path)
			require.NoError(t, err)

			value, err := nodeConfigArgsValue(string(data))
			require.NoError(t, err)
			require.Equal(t, tc.wantValue, value)

			// The writer must preserve every unrelated byte: the header comment
			// and the [Service] stanza survive, and only the provider tokens in
			// the KUBELET_NODE_CONFIG_ARGS line change.
			require.Contains(t, string(data), "# Managed by unbounded-agent. Do not edit directly.")
			require.Contains(t, string(data), "[Service]")

			if tc.wantErr {
				// A refused apply must not have written anything; there is no
				// idempotence pass to check because no change was made.
				require.Equal(t, tc.base, string(data), "a refused apply must not modify the drop-in")

				return
			}

			// Idempotence: a second pass with the same policy makes no change.
			changed2, err := applyProviderToNodeConfig(nil, isolation.ModeNone, tc.policy)
			require.NoError(t, err)
			require.False(t, changed2, "second pass must be a no-op for %s", tc.name)
		})
	}
}

func TestEffectiveManagedFlags(t *testing.T) {
	t.Parallel()

	m0, m1 := managedFlags()[0], managedFlags()[1]

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
			name: "managed flags present but a later value overrides them",
			argv: []string{"kubelet", m0, m1, "--image-credential-provider-config=/other", "--image-credential-provider-bin-dir=/other"},
			want: false,
		},
		{
			name: "only config flag present",
			argv: []string{"kubelet", m0},
			want: false,
		},
		{
			name: "no provider flags",
			argv: []string{"kubelet", "--hostname-override=n1"},
			want: false,
		},
		{
			name: "managed flags with unrelated flags after them",
			argv: []string{"kubelet", m0, m1, "--hostname-override=n1"},
			want: true,
		},
		{
			// Finding 3: a later SPLIT-FORM config flag overrides the managed
			// inline one; the effective value is the split-form value, so the
			// managed provider is not in effect.
			name: "later split-form config flag overrides managed inline",
			argv: []string{"kubelet", m0, m1, "--image-credential-provider-config", "/customer/config"},
			want: false,
		},
		{
			// Finding 3: a later SPLIT-FORM bin-dir flag overrides the managed
			// inline one.
			name: "later split-form bin-dir flag overrides managed inline",
			argv: []string{"kubelet", m0, m1, "--image-credential-provider-bin-dir", "/customer/bin"},
			want: false,
		},
		{
			// The split-form managed flags (flag and value as separate tokens)
			// are effective when their values match the managed values.
			name: "split-form managed flags are effective",
			argv: []string{"kubelet", "--image-credential-provider-config", goalstates.RegistryAuthCredentialConfigPath, "--image-credential-provider-bin-dir", goalstates.RegistryAuthProviderBinDir},
			want: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, registryAuthEffectiveManagedFlags(tc.argv))
		})
	}
}
