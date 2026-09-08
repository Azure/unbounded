// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResetAgentResourcesIncludesBPFFSMountCleanup(t *testing.T) {
	t.Parallel()

	taskName := ResetAgentResources(slog.New(slog.DiscardHandler)).Name()

	assert.Contains(t, taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)")
	assert.Less(t, strings.Index(taskName, "parallel(remove-machine, remove-machine)"), strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"))
	assert.Less(t, strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"), strings.Index(taskName, "cleanup-routes"))
}
