// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildPXEDeploymentUsesReplicas(t *testing.T) {
	t.Parallel()

	deploy := buildPXEDeployment(deployPXEParams{
		Site:     "boulderlab",
		Image:    "metalman:test",
		Replicas: 3,
	})

	require.NotNil(t, deploy.Spec)
	require.NotNil(t, deploy.Spec.Replicas)
	require.Equal(t, int32(3), *deploy.Spec.Replicas)
}

func TestDeployPXECommandReplicasDefault(t *testing.T) {
	t.Parallel()

	cmd := deployPXECommand()
	flag := cmd.Flags().Lookup("replicas")

	require.NotNil(t, flag)
	require.Equal(t, "1", flag.DefValue)
}
