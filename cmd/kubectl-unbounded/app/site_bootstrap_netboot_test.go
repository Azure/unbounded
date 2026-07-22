// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSiteBootstrapNetbootCommandContract(t *testing.T) {
	group := siteCommandGroup()

	cmd, _, err := group.Find([]string{"bootstrap-netboot"})
	require.NoError(t, err)
	require.Equal(t, "bootstrap-netboot SITE", cmd.Use)

	for _, name := range []string{
		"machine",
		"interface",
		"address",
		"endpoint-name",
		"http-port",
		"kubeconfig",
		"namespace",
		"metalman-binary",
		"timeout",
		"routed-cidr",
	} {
		require.NotNilf(t, cmd.Flags().Lookup(name), "missing --%s", name)
	}

	for _, name := range []string{"machine", "interface", "address"} {
		flag := cmd.Flags().Lookup(name)
		require.Contains(t, flag.Annotations, "cobra_annotation_bash_completion_one_required_flag")
	}
}
