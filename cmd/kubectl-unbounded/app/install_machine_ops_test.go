// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineOpsRBACTemplateIncludesBootstrapConfigMaps(t *testing.T) {
	rbac, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "machine-ops", "02-rbac.yaml.tmpl"))
	require.NoError(t, err)
	require.Contains(t, string(rbac), "cluster-info")
	require.Contains(t, string(rbac), "kube-root-ca.crt")
	require.Contains(t, string(rbac), "aks-cluster-metadata")
}
