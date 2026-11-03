// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperatorMakeTargetBuildsWholePackage(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	_, target, ok := strings.Cut(string(data), "\nunbounded-operator-build:")
	require.True(t, ok)

	recipe, _, _ := strings.Cut(target, "\n\n")
	require.Contains(t, recipe, "$(UNBOUNDED_OPERATOR_CMD)")
	require.NotContains(t, recipe, "$(UNBOUNDED_OPERATOR_CMD)/main.go", "image builds must include the provider and node-helper command files")
}
