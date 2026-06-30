// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestOCIReconcilerMapMachineToImage(t *testing.T) {
	tests := []struct {
		name       string
		r          *OCIReconciler
		machine    *v1alpha3.Machine
		wantImages []string
	}{
		{
			name: "explicit netboot image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-explicit"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:        "ghcr.io/test/machine:v1",
					NetbootImage: "ghcr.io/test/netboot:v1",
				}},
			},
			wantImages: []string{"ghcr.io/test/machine:v1", "ghcr.io/test/netboot:v1"},
		},
		{
			name: "default netboot image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-default"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image: "ghcr.io/test/machine:v1",
				}},
			},
			wantImages: []string{"ghcr.io/test/machine:v1", "ghcr.io/test/default-netboot:v1"},
		},
		{
			name: "dedupe same image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/machine:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-dedupe"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image: "ghcr.io/test/machine:v1",
				}},
			},
			wantImages: []string{"ghcr.io/test/machine:v1"},
		},
		{
			name: "no pxe",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-no-pxe"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := tt.r.mapMachineToImage(t.Context(), tt.machine)
			if len(reqs) != len(tt.wantImages) {
				t.Fatalf("request count: got %d, want %d: %#v", len(reqs), len(tt.wantImages), reqs)
			}

			for i, want := range tt.wantImages {
				got := reqs[i].NamespacedName
				if got != (client.ObjectKey{Name: want}) {
					t.Errorf("request %d: got %q, want %q", i, got.Name, want)
				}
			}
		})
	}
}
