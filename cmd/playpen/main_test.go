// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRootCommandRoles(t *testing.T) {
	t.Parallel()

	cmd := newRootCommand()
	require.NotNil(t, findSubcommand(cmd, "server"))
	require.NotNil(t, findSubcommand(cmd, "client"))
	require.NotNil(t, findSubcommand(cmd, "version"))
}

func TestRoleArguments(t *testing.T) {
	t.Parallel()

	server := newServerCommand()
	require.NoError(t, server.Args(server, nil))
	require.Error(t, server.Args(server, []string{"unexpected"}))

	client := newClientCommand()
	require.Error(t, client.Args(client, nil))
	require.NoError(t, client.Args(client, []string{"dnsmasq", "--keep-in-foreground"}))
}

func findSubcommand(root *cobra.Command, name string) *cobra.Command {
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}

	return nil
}
