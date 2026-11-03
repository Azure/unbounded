// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperatorDoesNotImplementCredentialProvider(t *testing.T) {
	cmd := newCommand(func(context.Context, config) error { return nil })
	_, _, err := cmd.Find([]string{"acr-credential-provider"})
	require.Error(t, err, "kubelet must invoke upstream's executable, not the operator")
	node, _, err := cmd.Find([]string{"registry-auth-node"})
	require.NoError(t, err)
	require.NotNil(t, node.Flags().Lookup("provider-binary"))
	require.Nil(t, node.Flags().Lookup("token-file"), "the installer must not acquire ACR credentials")
}
