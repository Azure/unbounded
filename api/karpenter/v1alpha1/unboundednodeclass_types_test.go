// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestUnboundedNodeClassImplementsStatusObject(t *testing.T) {
	nodeClass := &UnboundedNodeClass{}
	require.True(t, nodeClass.StatusConditions().SetTrue(status.ConditionReady))
	require.True(t, nodeClass.StatusConditions().Get(status.ConditionReady).IsTrue())
}

func TestGeneratedCRD(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/karpenter-provider/crd/karpenter.unbounded-cloud.io_unboundednodeclasses.yaml")
	require.NoError(t, err)

	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(data, &crd))
	require.Equal(t, "karpenter.unbounded-cloud.io", crd["spec"].(map[string]any)["group"])
	require.Equal(t, "Cluster", crd["spec"].(map[string]any)["scope"])
}
