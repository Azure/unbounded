// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestControllerImageBuildMetadata(t *testing.T) {
	data, err := os.ReadFile("../../images/racer-controller/Containerfile")
	require.NoError(t, err)

	image := string(data)
	for field, argument := range map[string]string{"Version": "VERSION", "GitCommit": "GIT_COMMIT", "BuildTime": "BUILD_TIME"} {
		require.Contains(t, image, "ARG "+argument+"=")
		require.Contains(t, image, "-X github.com/Azure/unbounded/internal/version."+field+"=${"+argument+"}")
	}

	require.Contains(t, image, "-o /out/racer-controller ./cmd/racer-controller")
	require.Contains(t, image, "COPY --from=builder /out/racer-controller /usr/local/bin/racer-controller")
}
