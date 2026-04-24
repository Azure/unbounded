// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

type fakeSiteClient struct {
	items []unboundednetv1alpha1.Site
}

// List lists resources for tests.
func (f *fakeSiteClient) List(context.Context, metav1.ListOptions) (*unboundednetv1alpha1.SiteList, error) {
	return &unboundednetv1alpha1.SiteList{Items: f.items}, nil
}

// Get gets a resource for tests.
func (f *fakeSiteClient) Get(context.Context, string, metav1.GetOptions) (*unboundednetv1alpha1.Site, error) {
	return nil, nil
}

// Watch watches resources for tests.
func (f *fakeSiteClient) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}

// UpdateStatus updates status for tests.
func (f *fakeSiteClient) UpdateStatus(context.Context, *unboundednetv1alpha1.Site, metav1.UpdateOptions) (*unboundednetv1alpha1.Site, error) {
	return nil, nil
}

type fakePoolClient struct {
	items []unboundednetv1alpha1.GatewayPool
}

// List lists resources for tests.
func (f *fakePoolClient) List(context.Context, metav1.ListOptions) (*unboundednetv1alpha1.GatewayPoolList, error) {
	return &unboundednetv1alpha1.GatewayPoolList{Items: f.items}, nil
}

// Get gets a resource for tests.
func (f *fakePoolClient) Get(context.Context, string, metav1.GetOptions) (*unboundednetv1alpha1.GatewayPool, error) {
	return nil, nil
}

// Watch watches resources for tests.
func (f *fakePoolClient) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}

// TestValidateHealthCheckSettings_IntervalSupportsIntMilliseconds tests validate health check settings interval supports int milliseconds.
func TestValidateHealthCheckSettings_IntervalSupportsIntMilliseconds(t *testing.T) {
	settings := &unboundednetv1alpha1.HealthCheckSettings{
		ReceiveInterval:  intstrPtr(300),
		TransmitInterval: intstrPtr(500),
	}

	if err := validateHealthCheckSettings(settings); err != nil {
		t.Fatalf("expected integer millisecond intervals to be valid, got error: %v", err)
	}
}

// TestValidateHealthCheckSettings_RejectsMalformedIntervalString tests validate health check settings rejects malformed interval string.
func TestValidateHealthCheckSettings_RejectsMalformedIntervalString(t *testing.T) {
	settings := &unboundednetv1alpha1.HealthCheckSettings{
		ReceiveInterval: intstrStringPtr("not-a-duration"),
	}

	err := validateHealthCheckSettings(settings)
	if err == nil {
		t.Fatalf("expected malformed interval string to be rejected")
	}

	if !strings.Contains(err.Error(), "valid duration string") {
		t.Fatalf("expected duration format hint in error, got: %v", err)
	}
}

func intstrPtr(v int) *intstr.IntOrString {
	value := intstr.FromInt(v)
	return &value
}

func intstrStringPtr(v string) *intstr.IntOrString {
	value := intstr.FromString(v)
	return &value
}

func newGatewayPoolRequest(t *testing.T, pool unboundednetv1alpha1.GatewayPool) *admissionv1.AdmissionRequest {
	t.Helper()

	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("failed to marshal gateway pool: %v", err)
	}

	return &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}
}

// TestValidateGatewayPool_RejectsInvalidHealthCheckSettings tests validate gateway pool rejects invalid health check settings.
func TestValidateGatewayPool_RejectsInvalidHealthCheckSettings(t *testing.T) {
	validator := &Validator{}
	pool := unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			NodeSelector: map[string]string{"role": "gateway"},
			HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{
				ReceiveInterval: intstrStringPtr("not-a-duration"),
			},
		},
	}

	resp := validator.validateGatewayPool(context.Background(), newGatewayPoolRequest(t, pool))
	if resp.Allowed {
		t.Fatalf("expected gateway pool validation to reject malformed healthCheckSettings")
	}

	if resp.Result == nil || !strings.Contains(resp.Result.Message, "healthCheckSettings.receiveInterval") {
		t.Fatalf("expected healthCheckSettings validation error, got: %#v", resp.Result)
	}
}

// TestValidateGatewayPool_AllowsValidHealthCheckSettings tests validate gateway pool allows valid health check settings.
func TestValidateGatewayPool_AllowsValidHealthCheckSettings(t *testing.T) {
	validator := &Validator{}
	pool := unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			NodeSelector: map[string]string{"role": "gateway"},
			HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{
				DetectMultiplier: ptrInt32(5),
				ReceiveInterval:  intstrPtr(300),
				TransmitInterval: intstrStringPtr("450ms"),
			},
		},
	}

	resp := validator.validateGatewayPool(context.Background(), newGatewayPoolRequest(t, pool))
	if !resp.Allowed {
		t.Fatalf("expected gateway pool validation to allow valid healthCheckSettings, got: %#v", resp.Result)
	}
}

// TestValidateSitePeering_RejectsUnknownSite tests validate site peering rejects unknown site.
func TestValidateSitePeering_RejectsUnknownSite(t *testing.T) {
	validator := &Validator{
		siteClient: &fakeSiteClient{items: []unboundednetv1alpha1.Site{
			{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}},
		}},
	}

	peering := unboundednetv1alpha1.SitePeering{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-a"},
		Spec: unboundednetv1alpha1.SitePeeringSpec{
			Sites: []string{"site-a", "site-c"},
		},
	}

	raw, err := json.Marshal(peering)
	if err != nil {
		t.Fatalf("failed to marshal SitePeering: %v", err)
	}

	resp := validator.validateSitePeering(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if resp.Allowed {
		t.Fatalf("expected SitePeering with unknown site to be denied")
	}
}

// TestValidateSiteGatewayPoolAssignment_RejectsUnknownPool tests validate site gateway pool assignment rejects unknown pool.
func TestValidateSiteGatewayPoolAssignment_RejectsUnknownPool(t *testing.T) {
	validator := &Validator{
		siteClient: &fakeSiteClient{items: []unboundednetv1alpha1.Site{
			{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
		}},
		poolClient: &fakePoolClient{items: []unboundednetv1alpha1.GatewayPool{
			{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}},
		}},
	}

	assignment := unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "assign-a"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites:        []string{"site-a"},
			GatewayPools: []string{"pool-b"},
		},
	}

	raw, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("failed to marshal SiteGatewayPoolAssignment: %v", err)
	}

	resp := validator.validateSiteGatewayPoolAssignment(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if resp.Allowed {
		t.Fatalf("expected SiteGatewayPoolAssignment with unknown pool to be denied")
	}
}

// TestValidateGatewayPoolPeering_RequiresTwoPools tests that the webhook
// allows a GatewayPoolPeering with fewer than 2 pools -- the CRD schema's
// minItems constraint rejects it before the webhook runs.
func TestValidateGatewayPoolPeering_RequiresTwoPools(t *testing.T) {
	validator := &Validator{}

	peering := unboundednetv1alpha1.GatewayPoolPeering{
		ObjectMeta: metav1.ObjectMeta{Name: "gpp-a"},
		Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{
			GatewayPools: []string{"pool-a"},
		},
	}

	raw, err := json.Marshal(peering)
	if err != nil {
		t.Fatalf("failed to marshal GatewayPoolPeering: %v", err)
	}

	resp := validator.validateGatewayPoolPeering(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	// The webhook no longer checks minItems; CRD schema handles it.
	if !resp.Allowed {
		t.Fatalf("expected GatewayPoolPeering with one pool to be allowed by webhook (CRD schema enforces minItems)")
	}
}

func ptrInt32(v int32) *int32 {
	return &v
}

// TestSplitCIDRBlocksAndResolveMaskSizes tests split cidrblocks and resolve mask sizes.
func TestSplitCIDRBlocksAndResolveMaskSizes(t *testing.T) {
	ipv4, ipv6, err := splitCIDRBlocks([]string{"10.0.0.0/16", "fd00::/64"})
	if err != nil {
		t.Fatalf("splitCIDRBlocks returned error: %v", err)
	}

	if len(ipv4) != 1 || len(ipv6) != 1 {
		t.Fatalf("unexpected family split: ipv4=%d ipv6=%d", len(ipv4), len(ipv6))
	}

	v4Mask, v6Mask := resolveMaskSizes(nil, ipv4, ipv6)
	if v4Mask != 24 || v6Mask != 80 {
		t.Fatalf("unexpected default masks: ipv4=%d ipv6=%d", v4Mask, v6Mask)
	}

	b := &unboundednetv1alpha1.NodeBlockSizes{IPv4: 26, IPv6: 96}

	v4Mask, v6Mask = resolveMaskSizes(b, ipv4, ipv6)
	if v4Mask != 26 || v6Mask != 96 {
		t.Fatalf("unexpected explicit masks: ipv4=%d ipv6=%d", v4Mask, v6Mask)
	}

	if _, _, err := splitCIDRBlocks([]string{"bad-cidr"}); err == nil {
		t.Fatalf("expected splitCIDRBlocks to fail on invalid CIDR")
	}
}

// TestCollectAndValidateCIDROverlapHelpers tests collect and validate cidroverlap helpers.
func TestCollectAndValidateCIDROverlapHelpers(t *testing.T) {
	sites := []unboundednetv1alpha1.Site{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"10.10.0.0/16"},
				PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
					AssignmentEnabled: boolPtr(true),
					CidrBlocks:        []string{"10.20.0.0/16"},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "site-b"},
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"10.11.0.0/16"},
				PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
					AssignmentEnabled: boolPtr(true),
					CidrBlocks:        []string{"10.21.0.0/16"},
				}},
			},
		},
	}

	if _, err := collectNodeCIDRs(sites); err != nil {
		t.Fatalf("collectNodeCIDRs returned error: %v", err)
	}

	if _, err := collectPodCIDRs(sites); err != nil {
		t.Fatalf("collectPodCIDRs returned error: %v", err)
	}

	if err := validateNodeCIDRsNoOverlap(sites); err != nil {
		t.Fatalf("validateNodeCIDRsNoOverlap returned error: %v", err)
	}

	if err := validatePodCIDRsNoOverlap(sites); err != nil {
		t.Fatalf("validatePodCIDRsNoOverlap returned error: %v", err)
	}

	overlapSites := []unboundednetv1alpha1.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.10.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.10.1.0/24"}}},
	}
	if err := validateNodeCIDRsNoOverlap(overlapSites); err == nil {
		t.Fatalf("expected overlap validation error")
	}
}

// TestDecodeSiteFromRequest tests decode site from request.
func TestDecodeSiteFromRequest(t *testing.T) {
	site := unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}}

	raw, err := json.Marshal(site)
	if err != nil {
		t.Fatalf("marshal site: %v", err)
	}

	req := &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}

	decodedSite, err := decodeSiteFromRequest(req)
	if err != nil || decodedSite.Name != "site-a" {
		t.Fatalf("decodeSiteFromRequest unexpected result: %#v err=%v", decodedSite, err)
	}

	req = &admissionv1.AdmissionRequest{Operation: admissionv1.Create}
	if _, err := decodeSiteFromRequest(req); err == nil {
		t.Fatalf("expected decodeSiteFromRequest to fail on missing object")
	}
}

// TestAssignmentEnabledAndIPFamily tests assignment enabled and ipfamily.
func TestAssignmentEnabledAndIPFamily(t *testing.T) {
	if !assignmentEnabled(nil) {
		t.Fatalf("expected nil assignmentEnabled to default true")
	}

	if assignmentEnabled(boolPtr(false)) {
		t.Fatalf("expected explicit false assignmentEnabled")
	}

	if !assignmentEnabled(boolPtr(true)) {
		t.Fatalf("expected explicit true assignmentEnabled")
	}

	_, v4, _ := net.ParseCIDR("10.0.0.0/24")

	_, v6, _ := net.ParseCIDR("fd00::/64")
	if ipFamily(v4) != "ipv4" || ipFamily(v6) != "ipv6" {
		t.Fatalf("unexpected ip families: %s %s", ipFamily(v4), ipFamily(v6))
	}
}

// TestDecodeGatewayPoolAndPeeringRequests tests decode gateway pool and peering requests.
func TestDecodeGatewayPoolAndPeeringRequests(t *testing.T) {
	pool := unboundednetv1alpha1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}}

	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}

	req := &admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}

	decodedPool, err := decodeGatewayPoolFromRequest(req)
	if err != nil || decodedPool.Name != "pool-a" {
		t.Fatalf("decodeGatewayPoolFromRequest unexpected result: %#v err=%v", decodedPool, err)
	}

	if _, err := decodeGatewayPoolFromRequest(&admissionv1.AdmissionRequest{Operation: admissionv1.Create}); err == nil {
		t.Fatalf("expected decodeGatewayPoolFromRequest to fail on missing object")
	}

	sp := unboundednetv1alpha1.SitePeering{ObjectMeta: metav1.ObjectMeta{Name: "peer-a"}}
	raw, _ = json.Marshal(sp)
	req = &admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}

	decodedSP, err := decodeSitePeeringFromRequest(req)
	if err != nil || decodedSP.Name != "peer-a" {
		t.Fatalf("decodeSitePeeringFromRequest unexpected result: %#v err=%v", decodedSP, err)
	}

	asg := unboundednetv1alpha1.SiteGatewayPoolAssignment{ObjectMeta: metav1.ObjectMeta{Name: "asg-a"}}
	raw, _ = json.Marshal(asg)
	req = &admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}

	decodedAsg, err := decodeSiteGatewayPoolAssignmentFromRequest(req)
	if err != nil || decodedAsg.Name != "asg-a" {
		t.Fatalf("decodeSiteGatewayPoolAssignmentFromRequest unexpected result: %#v err=%v", decodedAsg, err)
	}

	gpp := unboundednetv1alpha1.GatewayPoolPeering{ObjectMeta: metav1.ObjectMeta{Name: "gpp-a"}}
	raw, _ = json.Marshal(gpp)
	req = &admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}

	decodedGPP, err := decodeGatewayPoolPeeringFromRequest(req)
	if err != nil || decodedGPP.Name != "gpp-a" {
		t.Fatalf("decodeGatewayPoolPeeringFromRequest unexpected result: %#v err=%v", decodedGPP, err)
	}
}

// TestValidateCIDROverlapAllowSameSite tests validate cidroverlap allow same site.
func TestValidateCIDROverlapAllowSameSite(t *testing.T) {
	_, aCIDR, _ := net.ParseCIDR("10.0.0.0/16")
	_, bCIDR, _ := net.ParseCIDR("10.0.1.0/24")

	entries := []cidrEntry{
		{siteName: "site-a", label: "a", cidr: aCIDR, cidrStr: aCIDR.String()},
		{siteName: "site-a", label: "b", cidr: bCIDR, cidrStr: bCIDR.String()},
	}
	if err := validateCIDROverlap(entries, true, "nodeCIDR"); err != nil {
		t.Fatalf("expected same-site overlap to be allowed, got: %v", err)
	}

	if err := validateCIDROverlap(entries, false, "nodeCIDR"); err == nil {
		t.Fatalf("expected overlap error when same-site overlaps are not allowed")
	}
}

func boolPtr(v bool) *bool {
	return &v
}

// TestValidateSiteSpecAndMergeSiteList tests validate site spec and merge site list.
func TestValidateSiteSpecAndMergeSiteList(t *testing.T) {
	// Empty nodeCidrs is now rejected by CRD schema (minItems: 1), so the
	// webhook allows it through.
	if err := validateSiteSpec(unboundednetv1alpha1.Site{}); err != nil {
		t.Fatalf("expected validateSiteSpec to allow empty node CIDRs (CRD schema enforces minItems)")
	}

	site := unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundednetv1alpha1.SiteSpec{
			NodeCidrs: []string{"10.0.0.0/16"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
				AssignmentEnabled: boolPtr(false),
				CidrBlocks:        nil,
			}},
		},
	}
	if err := validateSiteSpec(site); err != nil {
		t.Fatalf("expected disabled assignment to be skipped, got %v", err)
	}

	site.Spec.LocalCIDRs = []string{"invalid"}
	if err := validateSiteSpec(site); err == nil {
		t.Fatalf("expected invalid local CIDR to be rejected")
	}

	existing := []unboundednetv1alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}}}

	merged := mergeSiteList(existing, unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}}, admissionv1.Create)
	if len(merged) != 2 {
		t.Fatalf("expected create merge to append new site, got %#v", merged)
	}

	merged = mergeSiteList(existing, unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.1.0.0/16"}}}, admissionv1.Update)
	if len(merged) != 1 || len(merged[0].Spec.NodeCidrs) != 1 {
		t.Fatalf("expected update merge to replace existing site, got %#v", merged)
	}

	merged = mergeSiteList(existing, unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-c"}}, admissionv1.Delete)
	if len(merged) != 1 || merged[0].Name != "site-a" {
		t.Fatalf("expected delete merge to return existing list unchanged, got %#v", merged)
	}
}

// TestValidateRequestAndHandleValidate tests validate request and handle validate.
func TestValidateRequestAndHandleValidate(t *testing.T) {
	s := &Server{validator: &Validator{}}

	resp := s.validateRequest(context.Background(), nil)
	if resp.Allowed {
		t.Fatalf("expected nil admission request to be denied")
	}

	pool := unboundednetv1alpha1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}}

	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal gateway pool: %v", err)
	}
	// GatewayPool with empty nodeSelector is now rejected by CRD schema
	// (minProperties: 1), so the webhook allows it through.
	resp = s.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource:  metav1.GroupVersionResource{Resource: "gatewaypools"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("expected gateway pool with empty nodeSelector to be allowed by webhook (CRD schema enforces minProperties)")
	}

	resp = s.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource: metav1.GroupVersionResource{Resource: "unknown"},
	})
	if !resp.Allowed {
		t.Fatalf("expected unknown resources to be allowed")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/validate", nil)
	s.handleValidate(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET /validate to return 405, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/validate", bytes.NewBufferString("not-json"))
	s.handleValidate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid body POST /validate to return 400, got %d", rec.Code)
	}

	uid := types.UID("1234")

	reviewBytes, err := json.Marshal(admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{
		UID:       uid,
		Resource:  metav1.GroupVersionResource{Resource: "unknown"},
		Operation: admissionv1.Create,
	}})
	if err != nil {
		t.Fatalf("marshal admission review: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
	s.handleValidate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected valid POST /validate to return 200, got %d", rec.Code)
	}

	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to decode admission response: %v", err)
	}

	if out.Response == nil || out.Response.UID != uid || !out.Response.Allowed {
		t.Fatalf("unexpected admission response: %#v", out.Response)
	}
}

// TestValidateSiteCreateAndDelete tests validate site create and delete.
func TestValidateSiteCreateAndDelete(t *testing.T) {
	client := kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"net.unbounded-cloud.io/site": "site-a"}}})
	validator := &Validator{
		clientset: client,
		siteClient: &fakeSiteClient{items: []unboundednetv1alpha1.Site{
			{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.1.0.0/16"}}},
		}},
	}

	site := unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec:       unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.0.0.0/16"}},
	}

	raw, err := json.Marshal(site)
	if err != nil {
		t.Fatalf("marshal site: %v", err)
	}

	resp := validator.validateSite(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("expected valid site create to be allowed, got %#v", resp.Result)
	}

	// DELETE is always allowed -- finalizers handle protection now
	resp = validator.validateSite(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("expected site delete to be allowed (finalizer protects), got %#v", resp.Result)
	}
}

// TestValidateSiteNodeSliceDelete tests that SiteNodeSlice operations are always
// allowed by the webhook. Deletion protection is handled by ownerReferences
// with blockOwnerDeletion.
func TestValidateSiteNodeSliceDelete(t *testing.T) {
	validator := &Validator{}

	resp := validator.validateSiteNodeSlice(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
	})
	if !resp.Allowed {
		t.Fatalf("expected sitenodeslice delete to be allowed (ownerReferences protect), got %#v", resp.Result)
	}

	resp = validator.validateSiteNodeSlice(context.Background(), &admissionv1.AdmissionRequest{Operation: admissionv1.Create})
	if !resp.Allowed {
		t.Fatalf("expected non-delete sitenodeslice operation to be allowed")
	}
}

// TestValidateGatewayPoolDeleteAndCIDRValidation tests validate gateway pool delete and cidrvalidation.
func TestValidateGatewayPoolDeleteAndCIDRValidation(t *testing.T) {
	validator := &Validator{clientset: kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"role": "gateway"}}})}
	pool := unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
	}

	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal gateway pool: %v", err)
	}

	// DELETE is always allowed -- finalizers handle protection now
	resp := validator.validateGatewayPool(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("expected gateway pool delete to be allowed (finalizer protects), got %#v", resp.Result)
	}

	// Invalid type is now rejected by CRD schema (enum), so the webhook
	// allows it through.
	invalidType := pool
	invalidType.Spec.Type = "bad"

	resp = validator.validateGatewayPool(context.Background(), newGatewayPoolRequest(t, invalidType))
	if !resp.Allowed {
		t.Fatalf("expected invalid gateway pool type to be allowed by webhook (CRD schema enforces enum)")
	}

	invalidRoutedCIDR := pool
	invalidRoutedCIDR.Spec.RoutedCidrs = []string{"invalid"}

	resp = validator.validateGatewayPool(context.Background(), newGatewayPoolRequest(t, invalidRoutedCIDR))
	if resp.Allowed {
		t.Fatalf("expected invalid routed CIDR to be denied")
	}
}

// TestValidateGatewayPoolPeeringPoolLookup tests validate gateway pool peering pool lookup.
func TestValidateGatewayPoolPeeringPoolLookup(t *testing.T) {
	validator := &Validator{poolClient: &fakePoolClient{items: []unboundednetv1alpha1.GatewayPool{
		{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pool-b"}},
	}}}

	peering := unboundednetv1alpha1.GatewayPoolPeering{
		ObjectMeta: metav1.ObjectMeta{Name: "gpp-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolPeeringSpec{GatewayPools: []string{"pool-a", "pool-x"}},
	}

	raw, err := json.Marshal(peering)
	if err != nil {
		t.Fatalf("failed to marshal GatewayPoolPeering: %v", err)
	}

	resp := validator.validateGatewayPoolPeering(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if resp.Allowed {
		t.Fatalf("expected unknown referenced gateway pool to be denied")
	}

	peering.Spec.GatewayPools = []string{"pool-a", "pool-b"}

	raw, err = json.Marshal(peering)
	if err != nil {
		t.Fatalf("failed to marshal GatewayPoolPeering: %v", err)
	}

	resp = validator.validateGatewayPoolPeering(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("expected valid gateway pool peering to be allowed, got %#v", resp.Result)
	}
}

// TestValidateRequestCoversResourceSwitch tests validate request covers resource switch.
func TestValidateRequestCoversResourceSwitch(t *testing.T) {
	server := &Server{validator: &Validator{clientset: kubefake.NewClientset()}}

	// sitepeerings delete path should allow.
	resp := server.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource:  metav1.GroupVersionResource{Resource: "sitepeerings"},
		Operation: admissionv1.Delete,
	})
	if !resp.Allowed {
		t.Fatalf("expected sitepeerings delete path to allow")
	}

	// sitegatewaypoolassignments delete path should allow.
	resp = server.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource:  metav1.GroupVersionResource{Resource: "sitegatewaypoolassignments"},
		Operation: admissionv1.Delete,
	})
	if !resp.Allowed {
		t.Fatalf("expected sitegatewaypoolassignments delete path to allow")
	}

	// gatewaypoolpeerings delete path should allow.
	resp = server.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource:  metav1.GroupVersionResource{Resource: "gatewaypoolpeerings"},
		Operation: admissionv1.Delete,
	})
	if !resp.Allowed {
		t.Fatalf("expected gatewaypoolpeerings delete path to allow")
	}

	// sitenodeslices create path should allow by default.
	resp = server.validateRequest(context.Background(), &admissionv1.AdmissionRequest{
		Resource:  metav1.GroupVersionResource{Resource: "sitenodeslices"},
		Operation: admissionv1.Create,
	})
	if !resp.Allowed {
		t.Fatalf("expected sitenodeslices create path to allow")
	}
}

// TestCidrsOverlap tests the cidrsOverlap helper function.
func TestCidrsOverlap(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		overlap bool
	}{
		{"superset overlap", "10.0.0.0/8", "10.1.0.0/16", true},
		{"subset overlap", "10.1.0.0/16", "10.0.0.0/8", true},
		{"exact match", "10.0.0.0/16", "10.0.0.0/16", true},
		{"no overlap", "10.0.0.0/16", "10.1.0.0/16", false},
		{"disjoint", "192.168.0.0/16", "10.0.0.0/8", false},
		{"invalid a", "bad", "10.0.0.0/8", false},
		{"invalid b", "10.0.0.0/8", "bad", false},
		{"ipv6 overlap", "fd00::/48", "fd00::/64", true},
		{"ipv6 no overlap", "fd00::/64", "fd01::/64", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cidrsOverlap(tt.a, tt.b)
			if got != tt.overlap {
				t.Errorf("cidrsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.overlap)
			}
		})
	}
}

// TestValidateIntraSiteCIDROverlap tests intra-site CIDR overlap detection.
func TestValidateIntraSiteCIDROverlap(t *testing.T) {
	// No overlap -- should pass
	site := unboundednetv1alpha1.Site{
		Spec: unboundednetv1alpha1.SiteSpec{
			NodeCidrs:          []string{"10.0.0.0/16"},
			NonMasqueradeCIDRs: []string{"172.16.0.0/16", "172.17.0.0/16"},
			LocalCIDRs:         []string{"192.168.0.0/24", "192.168.1.0/24"},
		},
	}
	if err := validateIntraSiteCIDROverlap(site); err != nil {
		t.Fatalf("expected no overlap error, got: %v", err)
	}

	// Overlap within NonMasqueradeCIDRs
	site.Spec.NonMasqueradeCIDRs = []string{"10.0.0.0/8", "10.1.0.0/16"}

	site.Spec.LocalCIDRs = nil
	if err := validateIntraSiteCIDROverlap(site); err == nil {
		t.Fatalf("expected overlap error within nonMasqueradeCIDRs")
	}

	// Overlap within LocalCIDRs
	site.Spec.NonMasqueradeCIDRs = nil

	site.Spec.LocalCIDRs = []string{"192.168.0.0/16", "192.168.1.0/24"}
	if err := validateIntraSiteCIDROverlap(site); err == nil {
		t.Fatalf("expected overlap error within localCidrs")
	}

	// Overlap between NonMasqueradeCIDRs and LocalCIDRs
	site.Spec.NonMasqueradeCIDRs = []string{"10.0.0.0/8"}

	site.Spec.LocalCIDRs = []string{"10.1.0.0/16"}
	if err := validateIntraSiteCIDROverlap(site); err == nil {
		t.Fatalf("expected overlap error between nonMasqueradeCIDRs and localCidrs")
	}
}
