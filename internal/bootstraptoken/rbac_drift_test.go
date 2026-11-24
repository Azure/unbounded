// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstraptoken

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// The kubelet's first client CSR is allowed only for identities bound by these
// ClusterRoleBindings. A Group the manifests do not bind still passes every
// unit test, but new nodes can no longer join.
func TestMachinaRBACBindsGroup(t *testing.T) {
	t.Parallel()

	out := t.TempDir()
	require.NoError(t, render.Render(filepath.Join(repoRoot(t), "deploy", "machina"), out, map[string]string{}))

	raw, err := os.ReadFile(filepath.Join(out, "07-bootstrapper-rbac.yaml"))
	require.NoError(t, err)

	bindings := map[string]rbacv1.ClusterRoleBinding{}

	for _, doc := range strings.Split(string(raw), "\n---") {
		var binding rbacv1.ClusterRoleBinding
		require.NoError(t, yaml.Unmarshal([]byte(doc), &binding))

		if binding.Kind == "ClusterRoleBinding" {
			bindings[binding.Name] = binding
		}
	}

	for _, name := range []string{"unbounded-bootstrapper-node", "unbounded-bootstrapper-node-autoapprove"} {
		binding, ok := bindings[name]
		require.True(t, ok, "rendered machina RBAC has no ClusterRoleBinding %q", name)
		require.Equal(t, []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: Group}}, binding.Subjects,
			"ClusterRoleBinding %q must bind Group", name)
	}
}

// repoRoot walks up from this test file's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed")

	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		require.NotEqual(t, dir, filepath.Dir(dir), "no go.mod above %s", file)
	}
}
