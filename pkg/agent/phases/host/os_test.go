// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageManagerForCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands []string
		want     string
		wantErr  bool
	}{
		{
			name:     "apt",
			commands: []string{"apt-get", "dpkg-query", "tdnf", "rpm"},
			want:     "apt-get",
		},
		{
			name:     "tdnf",
			commands: []string{"tdnf", "rpm"},
			want:     "tdnf",
		},
		{
			name:     "dnf",
			commands: []string{"dnf", "rpm"},
			want:     "dnf",
		},
		{
			name:     "unsupported",
			commands: []string{"rpm"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			has := map[string]bool{}
			for _, cmd := range tt.commands {
				has[cmd] = true
			}

			pm, err := packageManagerForCommands(func(name string) bool { return has[name] })
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, pm.name)
		})
	}
}
