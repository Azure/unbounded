// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateUserData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "bootstrap without users",
			content: `#cloud-config
packages:
- jq
write_files:
- path: /etc/example
  content: "users: is data here"
`,
		},
		{
			name: "reject users",
			content: `#cloud-config
users:
- name: temporary-admin
`,
			wantErr: "must not define top-level users",
		},
		{
			name: "reject Jinja users bypass",
			content: `## template: jinja
#cloud-config
"{{ 'users' }}":
- name: temporary-admin
`,
			wantErr: "cloud-init templating is not allowed",
		},
		{
			name:    "reject invalid yaml",
			content: "users: [",
			wantErr: "parse cloud-init user-data",
		},
		{
			name:    "reject empty",
			wantErr: "exactly one YAML mapping document",
		},
		{
			name: "reject second document",
			content: `#cloud-config
packages: [jq]
---
runcmd: [echo unexpected]
`,
			wantErr: "exactly one YAML document",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateUserData(strings.NewReader(tt.content))
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestRunRejectsJinjaTemplate(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := run([]string{"-"}, strings.NewReader(`## template: jinja
#cloud-config
"{{ 'users' }}":
- name: temporary-admin
`), &stderr)

	require.Equal(t, 1, exitCode)
	require.Contains(t, stderr.String(), "cloud-init templating is not allowed")
}

func TestRunReadsStdin(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := run([]string{"-"}, strings.NewReader("users: []\n"), &stderr)

	require.Equal(t, 1, exitCode)
	require.Contains(t, stderr.String(), "must not define top-level users")
}

func TestRunRejectsExtraArguments(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := run([]string{"one", "two"}, strings.NewReader(""), &stderr)

	require.Equal(t, 2, exitCode)
	require.Contains(t, stderr.String(), "usage:")
}
