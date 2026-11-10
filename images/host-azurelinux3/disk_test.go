// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostazurelinux3_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The disk script needs root file system inputs at fixed paths, so these
// tests cover only the settings it rejects before it reads any input.
func TestDiskRejectsUnsupportedSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     []string
		wantErr string
	}{
		{name: "missing architecture", env: nil, wantErr: "TARGETARCH is required"},
		{name: "unknown architecture", env: []string{"TARGETARCH=riscv64"}, wantErr: "unsupported architecture: riscv64"},
		{name: "unknown distribution", env: []string{"TARGETARCH=amd64", "DISTRO=fedora"}, wantErr: "unsupported distribution: fedora"},
		{name: "MSHV on Ubuntu", env: []string{"TARGETARCH=amd64", "DISTRO=ubuntu", "MSHV=1"}, wantErr: "MSHV images support Azure Linux only, not ubuntu"},
		{name: "MSHV on arm64", env: []string{"TARGETARCH=arm64", "MSHV=1"}, wantErr: "MSHV images support amd64 only, not arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("/bin/sh", "assets/disk.sh")
			command.Env = append([]string{"PATH=/usr/bin:/bin"}, test.env...)

			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.wantErr) {
				t.Fatalf("disk.sh error = %v, want %q\n%s", err, test.wantErr, output)
			}
		})
	}
}
