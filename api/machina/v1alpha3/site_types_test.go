// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestSiteResourceAndAddToScheme(t *testing.T) {
	gr := Resource("sites")
	if gr.Group != GroupVersion.Group || gr.Resource != "sites" {
		t.Fatalf("unexpected group resource: %#v", gr)
	}

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	for _, obj := range []runtime.Object{&Site{}, &SiteList{}} {
		kinds, _, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("ObjectKinds(%T) error = %v", obj, err)
		}

		if len(kinds) == 0 || kinds[0].Group != GroupVersion.Group || kinds[0].Version != GroupVersion.Version {
			t.Fatalf("unexpected GVK for %T: %#v", obj, kinds)
		}
	}
}

func TestDeepCopySiteAndList(t *testing.T) {
	enabled := true
	priority := int32(10)
	detectMultiplier := int32(3)
	receive := intstr.FromString("300ms")
	transmit := intstr.FromInt(400)

	site := &Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: SiteSpec{
			NodeCidrs: []string{"10.0.0.0/16"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{
					AssignmentEnabled: &enabled,
					CidrBlocks:        []string{"10.244.0.0/16"},
					NodeBlockSizes:    &unboundednetv1alpha1.NodeBlockSizes{IPv4: 24, IPv6: 80},
					NodeRegex:         []string{"^node-"},
					Priority:          &priority,
				},
			},
			ManageCniPlugin:    &enabled,
			NonMasqueradeCIDRs: []string{"172.16.0.0/12"},
			LocalCIDRs:         []string{"10.0.0.0/8"},
			HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{
				Enabled:          &enabled,
				DetectMultiplier: &detectMultiplier,
				ReceiveInterval:  &receive,
				TransmitInterval: &transmit,
			},
			Components: SiteComponents{
				Net: &SiteComponentSpec{Enabled: &enabled, Namespace: "unbounded-kube", Image: "net:v1"},
				Metalman: &MetalmanComponentSpec{
					SiteComponentSpec: SiteComponentSpec{Enabled: &enabled, Image: "metalman:v1"},
					DHCPAutoInterface: &enabled,
				},
			},
		},
		Status: SiteStatus{
			NodeCount:  2,
			SliceCount: 1,
			Components: map[string]SiteComponentStatus{
				"net": {Ready: true, Message: "ok"},
			},
		},
	}

	copied := site.DeepCopy()
	if copied == nil {
		t.Fatalf("DeepCopy() returned nil")
	}

	if copied.Name != "site-a" || copied.Spec.PodCidrAssignments[0].NodeBlockSizes.IPv4 != 24 {
		t.Fatalf("unexpected copied site: %#v", copied)
	}

	site.Spec.NodeCidrs[0] = "10.99.0.0/16"
	site.Spec.PodCidrAssignments[0].CidrBlocks[0] = "10.250.0.0/16"
	site.Spec.HealthCheckSettings.DetectMultiplier = ptrInt32(9)
	site.Status.Components["net"] = SiteComponentStatus{Ready: false}

	if copied.Spec.NodeCidrs[0] != "10.0.0.0/16" {
		t.Fatalf("expected deep-copied NodeCidrs to be isolated")
	}

	if copied.Spec.PodCidrAssignments[0].CidrBlocks[0] != "10.244.0.0/16" {
		t.Fatalf("expected deep-copied assignment CidrBlocks to be isolated")
	}

	if copied.Spec.HealthCheckSettings.DetectMultiplier == nil || *copied.Spec.HealthCheckSettings.DetectMultiplier != 3 {
		t.Fatalf("expected deep-copied health check settings to be isolated")
	}

	if !copied.Status.Components["net"].Ready {
		t.Fatalf("expected deep-copied component status to be isolated")
	}

	if site.DeepCopyObject() == nil {
		t.Fatalf("expected Site.DeepCopyObject() not nil")
	}

	siteList := &SiteList{Items: []Site{*site}}
	if got := siteList.DeepCopy(); got == nil || len(got.Items) != 1 {
		t.Fatalf("unexpected SiteList deepcopy result: %#v", got)
	}

	if siteList.DeepCopyObject() == nil {
		t.Fatalf("expected SiteList.DeepCopyObject() not nil")
	}
}

func TestComponentEnabled(t *testing.T) {
	if ComponentEnabled(nil) {
		t.Fatalf("nil component should default disabled")
	}

	if ComponentEnabled(&SiteComponentSpec{}) {
		t.Fatalf("component without explicit enabled should default disabled")
	}

	enabled := true
	if !ComponentEnabled(&SiteComponentSpec{Enabled: &enabled}) {
		t.Fatalf("component with enabled=true should be enabled")
	}

	disabled := false
	if ComponentEnabled(&SiteComponentSpec{Enabled: &disabled}) {
		t.Fatalf("component with enabled=false should be disabled")
	}
}

func ptrInt32(v int32) *int32 {
	return &v
}
