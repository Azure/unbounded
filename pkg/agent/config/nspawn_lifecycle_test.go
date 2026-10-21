// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNSpawnLifecycle(t *testing.T) {
	t.Parallel()

	const (
		preStart  = "/usr/bin/systemctl start app-pre-start@%i.service"
		postStart = "/usr/bin/systemctl start app-post-start@%i.service"
	)

	tests := map[string]struct {
		lifecycle *NSpawnLifecycleConfig
		wantErrs  []string
	}{
		"nil runs the helper": {},
		"both commands": {
			lifecycle: &NSpawnLifecycleConfig{PreStartCommand: preStart, PostStartCommand: postStart},
		},
		"missing pre-start command": {
			lifecycle: &NSpawnLifecycleConfig{PostStartCommand: postStart},
			wantErrs:  []string{"NSpawnLifecycle.PreStartCommand is required"},
		},
		"blank post-start command": {
			lifecycle: &NSpawnLifecycleConfig{PreStartCommand: preStart, PostStartCommand: "  "},
			wantErrs:  []string{"NSpawnLifecycle.PostStartCommand is required"},
		},
		"no commands": {
			lifecycle: &NSpawnLifecycleConfig{},
			wantErrs: []string{
				"NSpawnLifecycle.PreStartCommand is required",
				"NSpawnLifecycle.PostStartCommand is required",
			},
		},
		"a relative command": {
			lifecycle: &NSpawnLifecycleConfig{PreStartCommand: "systemctl start app-pre-start@%i.service", PostStartCommand: postStart},
			wantErrs:  []string{"NSpawnLifecycle.PreStartCommand must start with the command's absolute path"},
		},
		"a prefix that ignores failure": {
			lifecycle: &NSpawnLifecycleConfig{PreStartCommand: preStart, PostStartCommand: "-" + postStart},
			wantErrs:  []string{"NSpawnLifecycle.PostStartCommand must start with the command's absolute path"},
		},
		"a trailing backslash would join the next line": {
			lifecycle: &NSpawnLifecycleConfig{PreStartCommand: preStart, PostStartCommand: postStart + ` \ `},
			wantErrs:  []string{"NSpawnLifecycle.PostStartCommand must not end in a backslash"},
		},
		"a newline would add a unit directive": {
			lifecycle: &NSpawnLifecycleConfig{
				PreStartCommand:  preStart + "\nExecStartPost=/bin/sh -c id",
				PostStartCommand: postStart,
			},
			wantErrs: []string{"NSpawnLifecycle.PreStartCommand must not contain control characters"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateNSpawnLifecycle(tc.lifecycle)
			if len(tc.wantErrs) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			for _, want := range tc.wantErrs {
				require.ErrorContains(t, err, want)
			}
		})
	}
}
