// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestKubernetesSpecOmitsUnsetBootstrapTokenRef(t *testing.T) {
	spec := KubernetesSpec{
		NodeLabels: map[string]string{"example.com/test": "true"},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal KubernetesSpec: %v", err)
	}

	if !bytes.Contains(data, []byte(`"nodeLabels"`)) {
		t.Fatalf("marshaled KubernetesSpec = %s, want nodeLabels", data)
	}

	if bytes.Contains(data, []byte(`"bootstrapTokenRef"`)) {
		t.Fatalf("marshaled KubernetesSpec = %s, want bootstrapTokenRef omitted", data)
	}
}

func TestPXESpecTargetRAIDMode(t *testing.T) {
	tests := []struct {
		name string
		pxe  *PXESpec
		want string
	}{
		{
			name: "nil pxe defaults none",
			want: PXERAIDModeNone,
		},
		{
			name: "nil install defaults none",
			pxe:  &PXESpec{},
			want: PXERAIDModeNone,
		},
		{
			name: "empty install raid mode defaults none",
			pxe:  &PXESpec{Install: &PXEInstallSpec{}},
			want: PXERAIDModeNone,
		},
		{
			name: "explicit raid1",
			pxe:  &PXESpec{Install: &PXEInstallSpec{RAIDMode: PXERAIDModeRAID1}},
			want: PXERAIDModeRAID1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pxe.TargetRAIDMode(); got != tt.want {
				t.Fatalf("TargetRAIDMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPXESpecTargetInstallMode(t *testing.T) {
	tests := []struct {
		name string
		pxe  *PXESpec
		want string
	}{
		{
			name: "nil pxe defaults raw",
			want: PXEInstallModeRaw,
		},
		{
			name: "none raid mode maps to raw install",
			pxe:  &PXESpec{Install: &PXEInstallSpec{RAIDMode: PXERAIDModeNone}},
			want: PXEInstallModeRaw,
		},
		{
			name: "raid1 maps to raid1 install",
			pxe:  &PXESpec{Install: &PXEInstallSpec{RAIDMode: PXERAIDModeRAID1}},
			want: PXEInstallModeRAID1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pxe.TargetInstallMode(); got != tt.want {
				t.Fatalf("TargetInstallMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPXESpecInstallTargetDisks(t *testing.T) {
	tests := []struct {
		name string
		pxe  *PXESpec
		want []string
	}{
		{
			name: "nil pxe",
		},
		{
			name: "top-level target disks",
			pxe:  &PXESpec{TargetDisks: []string{"/dev/disk/by-id/a", "/dev/disk/by-id/b"}},
			want: []string{"/dev/disk/by-id/a", "/dev/disk/by-id/b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pxe.InstallTargetDisks()
			if len(got) != len(tt.want) {
				t.Fatalf("InstallTargetDisks() = %v, want %v", got, tt.want)
			}

			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("InstallTargetDisks() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPXESpecValidateInstall(t *testing.T) {
	tests := []struct {
		name    string
		pxe     *PXESpec
		wantErr bool
	}{
		{
			name: "nil pxe defaults raw",
		},
		{
			name: "raw allows no disks",
			pxe:  &PXESpec{},
		},
		{
			name: "raw allows explicit disk",
			pxe: &PXESpec{
				TargetDisks: []string{"/dev/disk/by-id/os"},
			},
		},
		{
			name: "raid1 requires disks",
			pxe: &PXESpec{
				Install: &PXEInstallSpec{RAIDMode: PXERAIDModeRAID1},
			},
			wantErr: true,
		},
		{
			name: "raid1 rejects one disk",
			pxe: &PXESpec{
				TargetDisks: []string{"/dev/disk/by-id/a"},
				Install: &PXEInstallSpec{
					RAIDMode: PXERAIDModeRAID1,
				},
			},
			wantErr: true,
		},
		{
			name: "raid1 accepts two disks",
			pxe: &PXESpec{
				TargetDisks: []string{"/dev/disk/by-id/a", "/dev/disk/by-id/b"},
				Install: &PXEInstallSpec{
					RAIDMode: PXERAIDModeRAID1,
				},
			},
		},
		{
			name: "raid1 rejects three disks",
			pxe: &PXESpec{
				TargetDisks: []string{"/dev/disk/by-id/a", "/dev/disk/by-id/b", "/dev/disk/by-id/c"},
				Install: &PXEInstallSpec{
					RAIDMode: PXERAIDModeRAID1,
				},
			},
			wantErr: true,
		},
		{
			name: "raid1 does not use a single top-level disk",
			pxe: &PXESpec{
				TargetDisks: []string{"/dev/disk/by-id/os"},
				Install:     &PXEInstallSpec{RAIDMode: PXERAIDModeRAID1},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pxe.ValidateInstall()
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateInstall() succeeded, want error")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateInstall() error = %v", err)
			}
		})
	}
}
