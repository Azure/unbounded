// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerRuntimeUserSupportsUDS(t *testing.T) {
	data, err := os.ReadFile("../../images/racer-loadgen/Containerfile")
	require.NoError(t, err)

	var base, user string

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch strings.ToUpper(fields[0]) {
		case "FROM":
			base, user = fields[1], ""
		case "USER":
			user = fields[1]
		}
	}

	require.Equal(t, "scratch", base)
	require.Equal(t, "0:0", user, "UDS startup needs the default root-owned /run/racer layout and an origin socket accessible to the root dataplane")
}
