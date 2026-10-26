// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"bytes"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMachineNodeName(t *testing.T) {
	tests := []struct {
		name string
		spec MachineSpec
		want string
	}{
		{name: "no kubernetes spec", want: "m1"},
		{name: "no node ref", spec: MachineSpec{Kubernetes: &KubernetesSpec{}}, want: "m1"},
		{name: "empty node ref", spec: MachineSpec{Kubernetes: &KubernetesSpec{NodeRef: &LocalObjectReference{}}}, want: "m1"},
		{name: "node ref", spec: MachineSpec{Kubernetes: &KubernetesSpec{NodeRef: &LocalObjectReference{Name: "node-a"}}}, want: "node-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Machine{ObjectMeta: metav1.ObjectMeta{Name: "m1"}, Spec: tt.spec}
			if got := m.NodeName(); got != tt.want {
				t.Fatalf("NodeName() = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestBootProtocolDefaultsAndVirtualMedia(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *PXESpec
		want string
	}{
		{"nil", nil, PXEBootProtocolPXE},
		{"default", &PXESpec{}, PXEBootProtocolPXE},
		{"pxe", &PXESpec{BootProtocol: PXEBootProtocolPXE}, PXEBootProtocolPXE},
		{"http", &PXESpec{BootProtocol: PXEBootProtocolHTTP}, PXEBootProtocolHTTP},
		{"media", &PXESpec{BootProtocol: PXEBootProtocolVirtualMedia}, PXEBootProtocolVirtualMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.TargetBootProtocol(); got != tc.want {
				t.Fatalf("protocol = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBootMediaStatusRoundTrip(t *testing.T) {
	original := MachineOperationTargetStatus{MachineRef: "machine", BootMedia: &MachineOperationBootMediaStatus{
		ID: "media", SourceServer: "10.0.0.1", Phase: "Installing", MachineUID: "machine-uid",
		Netboot: &PXESpec{BootProtocol: PXEBootProtocolVirtualMedia, Image: "disk", Redfish: &RedfishSpec{
			URL: "https://bmc", PasswordRef: SecretKeySelector{Name: "credentials", Namespace: "system", Key: "password"},
		}},
		CertFingerprint: "fingerprint", ImageWritten: true, Cleaned: true,
	}}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	var restored MachineOperationTargetStatus
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	b := restored.BootMedia
	if b == nil || b.ID != "media" || b.SourceServer != "10.0.0.1" || b.MachineUID != "machine-uid" || b.CertFingerprint != "fingerprint" || !b.ImageWritten || !b.Cleaned || b.Netboot.Redfish.PasswordRef.Name != "credentials" {
		t.Fatalf("media status changed during JSON round trip: %s", data)
	}
}
