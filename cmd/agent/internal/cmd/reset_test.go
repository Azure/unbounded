// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCmdReset_IsRegistered(t *testing.T) {
	t.Parallel()

	cmdCtx := &CommandContext{LogFormat: "text"}
	cmd := newCmdReset(cmdCtx)

	assert.Equal(t, "reset", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)

	flag := cmd.Flags().Lookup("machine-name")
	require.Nil(t, flag)
}
