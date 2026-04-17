// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/allocator"
	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	unboundednet "github.com/Azure/unbounded-kube/internal/net/client/unboundednet"
)

// Validator validates admission requests for unbounded CNI custom resources.
type Validator struct {
	clientset  kubernetes.Interface
	siteClient unboundednet.SiteInterface
	poolClient unboundednet.GatewayPoolInterface
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request", http.StatusBadRequest)
		return
	}

	defer func() { _ = r.Body.Close() }() //nolint:errcheck

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		http.Error(w, "failed to parse admission review", http.StatusBadRequest)
		return
	}

	resource := ""
	operation := ""

	if review.Request != nil {
		resource = review.Request.Resource.Resource
		operation = string(review.Request.Operation)
	}

	response := s.validateRequest(r.Context(), review.Request)

	review.Response = response
	if review.Request != nil {
		review.Response.UID = review.Request.UID
	}

	result := "allowed"
	if response != nil && !response.Allowed {
		result = "denied"
	}

	webhookRequestsTotal.WithLabelValues(resource, operation, result).Inc()
	webhookRequestDuration.WithLabelValues(resource, operation).Observe(time.Since(start).Seconds())

	payload, err := json.Marshal(review)
	if err != nil {
		http.Error(w, "failed to serialize response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(payload); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}

func (s *Server) validateRequest(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil {
		return denyResponse("empty admission request")
	}

	switch req.Resource.Resource {
	case "sites":
		return s.validator.validateSite(ctx, req)
	case "gatewaypools":
		return s.validator.validateGatewayPool(ctx, req)
	case "sitepeerings":
		return s.validator.validateSitePeering(ctx, req)
	case "sitegatewaypoolassignments":
		return s.validator.validateSiteGatewayPoolAssignment(ctx, req)
	case "gatewaypoolpeerings":
		return s.validator.validateGatewayPoolPeering(ctx, req)
	case "sitenodeslices":
		return s.validator.validateSiteNodeSlice(ctx, req)
	default:
		return allowResponse("")
	}
}

func (v *Validator) validateSite(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	site, err := decodeSiteFromRequest(req)
	if err != nil {
		return denyResponse(err.Error())
	}

	if req.Operation == admissionv1.Delete {
		return allowResponse("")
	}

	if err := validateSiteSpec(site); err != nil {
		return denyResponse(err.Error())
	}

	siteList, err := v.siteClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return denyResponse(fmt.Sprintf("failed to list sites: %v", err))
	}

	sites := mergeSiteList(siteList.Items, site, req.Operation)

	if err := validateNodeCIDRsNoOverlap(sites); err != nil {
		return denyResponse(err.Error())
	}

	if err := validatePodCIDRsNoOverlap(sites); err != nil {
		return denyResponse(err.Error())
	}

	return allowResponse("")
}

func (v *Validator) validateGatewayPool(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	pool, err := decodeGatewayPoolFromRequest(req)
	if err != nil {
		return denyResponse(err.Error())
	}

	if req.Operation == admissionv1.Delete {
		return allowResponse("")
	}

	for _, cidr := range pool.Spec.RoutedCidrs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return denyResponse(fmt.Sprintf("invalid spec.routedCidrs CIDR %q: %v", cidr, err))
		}
	}

	if err := validateHealthCheckSettings(pool.Spec.HealthCheckSettings); err != nil {
		return denyResponse(err.Error())
	}

	return allowResponse("")
}

func (v *Validator) validateSiteNodeSlice(_ context.Context, _ *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	return allowResponse("")
}

func (v *Validator) validateSitePeering(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Operation == admissionv1.Delete {
		return allowResponse("")
	}

	peering, err := decodeSitePeeringFromRequest(req)
	if err != nil {
		return denyResponse(err.Error())
	}

	if err := validateNamesList(peering.Spec.Sites, "spec.sites"); err != nil {
		return denyResponse(err.Error())
	}

	if err := validateHealthCheckSettings(peering.Spec.HealthCheckSettings); err != nil {
		return denyResponse(err.Error())
	}

	if v.siteClient != nil {
		sites, err := v.siteClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to list sites: %v", err))
		}

		siteSet := make(map[string]struct{}, len(sites.Items))
		for _, site := range sites.Items {
			siteSet[site.Name] = struct{}{}
		}

		for _, siteName := range peering.Spec.Sites {
			if _, exists := siteSet[siteName]; !exists {
				return denyResponse(fmt.Sprintf("spec.sites references unknown Site %q", siteName))
			}
		}
	}

	return allowResponse("")
}

func (v *Validator) validateSiteGatewayPoolAssignment(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Operation == admissionv1.Delete {
		return allowResponse("")
	}

	assignment, err := decodeSiteGatewayPoolAssignmentFromRequest(req)
	if err != nil {
		return denyResponse(err.Error())
	}

	if err := validateNamesList(assignment.Spec.Sites, "spec.sites"); err != nil {
		return denyResponse(err.Error())
	}

	if err := validateNamesList(assignment.Spec.GatewayPools, "spec.gatewayPools"); err != nil {
		return denyResponse(err.Error())
	}

	if err := validateHealthCheckSettings(assignment.Spec.HealthCheckSettings); err != nil {
		return denyResponse(err.Error())
	}

	if v.siteClient != nil {
		sites, err := v.siteClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to list sites: %v", err))
		}

		siteSet := make(map[string]struct{}, len(sites.Items))
		for _, site := range sites.Items {
			siteSet[site.Name] = struct{}{}
		}

		for _, siteName := range assignment.Spec.Sites {
			if _, exists := siteSet[siteName]; !exists {
				return denyResponse(fmt.Sprintf("spec.sites references unknown Site %q", siteName))
			}
		}
	}

	if v.poolClient != nil {
		pools, err := v.poolClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to list gateway pools: %v", err))
		}

		poolSet := make(map[string]struct{}, len(pools.Items))
		for _, pool := range pools.Items {
			poolSet[pool.Name] = struct{}{}
		}

		for _, poolName := range assignment.Spec.GatewayPools {
			if _, exists := poolSet[poolName]; !exists {
				return denyResponse(fmt.Sprintf("spec.gatewayPools references unknown GatewayPool %q", poolName))
			}
		}
	}

	return allowResponse("")
}

func (v *Validator) validateGatewayPoolPeering(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Operation == admissionv1.Delete {
		return allowResponse("")
	}

	peering, err := decodeGatewayPoolPeeringFromRequest(req)
	if err != nil {
		return denyResponse(err.Error())
	}

	if err := validateNamesList(peering.Spec.GatewayPools, "spec.gatewayPools"); err != nil {
		return denyResponse(err.Error())
	}

	if err := validateHealthCheckSettings(peering.Spec.HealthCheckSettings); err != nil {
		return denyResponse(err.Error())
	}

	if v.poolClient != nil {
		pools, err := v.poolClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to list gateway pools: %v", err))
		}

		poolSet := make(map[string]struct{}, len(pools.Items))
		for _, pool := range pools.Items {
			poolSet[pool.Name] = struct{}{}
		}

		for _, poolName := range peering.Spec.GatewayPools {
			if _, exists := poolSet[poolName]; !exists {
				return denyResponse(fmt.Sprintf("spec.gatewayPools references unknown GatewayPool %q", poolName))
			}
		}
	}

	return allowResponse("")
}

func validateNamesList(names []string, field string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("%s cannot contain empty names", field)
		}

		if _, exists := seen[name]; exists {
			return fmt.Errorf("%s contains duplicate entry %q", field, name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

func validateSiteSpec(site unboundednetv1alpha1.Site) error {
	for _, cidr := range site.Spec.NodeCidrs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid spec.nodeCidrs CIDR %q: %w", cidr, err)
		}
	}

	for _, cidr := range site.Spec.NonMasqueradeCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid spec.nonMasqueradeCIDRs CIDR %q: %w", cidr, err)
		}
	}

	for _, cidr := range site.Spec.LocalCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid spec.localCidrs CIDR %q: %w", cidr, err)
		}
	}

	if err := validateHealthCheckSettings(site.Spec.HealthCheckSettings); err != nil {
		return err
	}

	if err := validateIntraSiteCIDROverlap(site); err != nil {
		return err
	}

	ipv4Mask := 0
	ipv6Mask := 0

	for i, assignment := range site.Spec.PodCidrAssignments {
		if !assignmentEnabled(assignment.AssignmentEnabled) {
			continue
		}

		for _, pattern := range assignment.NodeRegex {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("invalid spec.podCidrAssignments[%d].nodeRegex %q: %w", i, pattern, err)
			}
		}

		ipv4Pools, ipv6Pools, err := splitCIDRBlocks(assignment.CidrBlocks)
		if err != nil {
			return fmt.Errorf("spec.podCidrAssignments[%d].cidrBlocks: %w", i, err)
		}

		mask4, mask6 := resolveMaskSizes(assignment.NodeBlockSizes, ipv4Pools, ipv6Pools)
		if len(ipv4Pools) > 0 {
			if mask4 < 16 || mask4 > 28 {
				return fmt.Errorf("spec.podCidrAssignments[%d] IPv4 mask size /%d must be between /16 and /28", i, mask4)
			}
		}

		if len(ipv6Pools) > 0 {
			for _, pool := range ipv6Pools {
				ones, _ := pool.Mask.Size()

				maxMask := ones + 16
				if mask6 > maxMask {
					return fmt.Errorf("spec.podCidrAssignments[%d] IPv6 mask size /%d must be no greater than /%d for pool %s", i, mask6, maxMask, pool.String())
				}
			}
		}

		if _, err := allocator.NewAllocator(ipv4Pools, ipv6Pools, mask4, mask6); err != nil {
			return fmt.Errorf("spec.podCidrAssignments[%d] has invalid mask sizes: %w", i, err)
		}

		if len(ipv4Pools) > 0 {
			if ipv4Mask == 0 {
				ipv4Mask = mask4
			} else if ipv4Mask != mask4 {
				return fmt.Errorf("spec.podCidrAssignments must use a consistent IPv4 mask size; expected /%d but got /%d", ipv4Mask, mask4)
			}
		}

		if len(ipv6Pools) > 0 {
			if ipv6Mask == 0 {
				ipv6Mask = mask6
			} else if ipv6Mask != mask6 {
				return fmt.Errorf("spec.podCidrAssignments must use a consistent IPv6 mask size; expected /%d but got /%d", ipv6Mask, mask6)
			}
		}
	}

	return nil
}

func validateHealthCheckSettings(settings *unboundednetv1alpha1.HealthCheckSettings) error {
	if settings == nil {
		return nil
	}

	if settings.ReceiveInterval != nil {
		if err := validateHealthCheckInterval("receiveInterval", *settings.ReceiveInterval); err != nil {
			return err
		}
	}

	if settings.TransmitInterval != nil {
		if err := validateHealthCheckInterval("transmitInterval", *settings.TransmitInterval); err != nil {
			return err
		}
	}

	return nil
}

func validateHealthCheckInterval(field string, value intstr.IntOrString) error {
	const (
		minInterval = 10 * time.Millisecond
		maxInterval = 4294967 * time.Millisecond
	)

	parsed, err := parseHealthCheckIntervalDuration(value)
	if err != nil {
		return fmt.Errorf("healthCheckSettings.%s must be a valid duration string (for example \"300ms\") or integer milliseconds", field)
	}

	if parsed < minInterval || parsed > maxInterval {
		return fmt.Errorf("healthCheckSettings.%s must be between %s and %s", field, minInterval, maxInterval)
	}

	return nil
}

func parseHealthCheckIntervalDuration(value intstr.IntOrString) (time.Duration, error) {
	switch value.Type {
	case intstr.Int:
		return time.Duration(value.IntValue()) * time.Millisecond, nil
	case intstr.String:
		raw := strings.TrimSpace(value.StrVal)
		if raw == "" {
			return 0, fmt.Errorf("empty interval")
		}

		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed, nil
		}

		if milliseconds, err := strconv.Atoi(raw); err == nil {
			return time.Duration(milliseconds) * time.Millisecond, nil
		}

		return 0, fmt.Errorf("invalid interval %q", raw)
	default:
		return 0, fmt.Errorf("unsupported interval type %v", value.Type)
	}
}

func splitCIDRBlocks(blocks []string) ([]*net.IPNet, []*net.IPNet, error) {
	var (
		ipv4Pools []*net.IPNet
		ipv6Pools []*net.IPNet
	)

	for _, block := range blocks {
		ip, ipNet, err := net.ParseCIDR(block)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid CIDR %q: %w", block, err)
		}

		if ip.To4() != nil {
			ipv4Pools = append(ipv4Pools, ipNet)
		} else {
			ipv6Pools = append(ipv6Pools, ipNet)
		}
	}

	return ipv4Pools, ipv6Pools, nil
}

func resolveMaskSizes(blockSizes *unboundednetv1alpha1.NodeBlockSizes, ipv4Pools, ipv6Pools []*net.IPNet) (int, int) {
	ipv4Mask := 0
	ipv6Mask := 0

	if blockSizes != nil {
		ipv4Mask = blockSizes.IPv4
		ipv6Mask = blockSizes.IPv6
	}

	if len(ipv4Pools) > 0 && ipv4Mask == 0 {
		ipv4Mask = 24
	}

	if len(ipv6Pools) > 0 && ipv6Mask == 0 {
		ones, _ := ipv6Pools[0].Mask.Size()

		ipv6Mask = ones + 16
		if ipv6Mask > 128 {
			ipv6Mask = 128
		}
	}

	return ipv4Mask, ipv6Mask
}

func validateNodeCIDRsNoOverlap(sites []unboundednetv1alpha1.Site) error {
	entries, err := collectNodeCIDRs(sites)
	if err != nil {
		return err
	}

	return validateCIDROverlap(entries, true, "nodeCIDR")
}

func validatePodCIDRsNoOverlap(sites []unboundednetv1alpha1.Site) error {
	entries, err := collectPodCIDRs(sites)
	if err != nil {
		return err
	}

	return validateCIDROverlap(entries, false, "podCIDR")
}

type cidrEntry struct {
	siteName string
	label    string
	cidr     *net.IPNet
	cidrStr  string
}

func collectNodeCIDRs(sites []unboundednetv1alpha1.Site) ([]cidrEntry, error) {
	var entries []cidrEntry

	for _, site := range sites {
		for _, cidrStr := range site.Spec.NodeCidrs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				return nil, fmt.Errorf("site %q has invalid nodeCIDR %q: %w", site.Name, cidrStr, err)
			}

			entries = append(entries, cidrEntry{
				siteName: site.Name,
				label:    "spec.nodeCidrs",
				cidr:     cidr,
				cidrStr:  cidr.String(),
			})
		}
	}

	return entries, nil
}

func collectPodCIDRs(sites []unboundednetv1alpha1.Site) ([]cidrEntry, error) {
	var entries []cidrEntry

	for _, site := range sites {
		for i, assignment := range site.Spec.PodCidrAssignments {
			if !assignmentEnabled(assignment.AssignmentEnabled) {
				continue
			}

			for _, cidrStr := range assignment.CidrBlocks {
				_, cidr, err := net.ParseCIDR(cidrStr)
				if err != nil {
					return nil, fmt.Errorf("site %q has invalid podCIDR %q: %w", site.Name, cidrStr, err)
				}

				entries = append(entries, cidrEntry{
					siteName: site.Name,
					label:    fmt.Sprintf("spec.podCidrAssignments[%d].cidrBlocks", i),
					cidr:     cidr,
					cidrStr:  cidr.String(),
				})
			}
		}
	}

	return entries, nil
}

func validateCIDROverlap(entries []cidrEntry, allowSameSite bool, cidrType string) error {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			a := entries[i]
			b := entries[j]

			if allowSameSite && a.siteName == b.siteName {
				continue
			}

			if ipFamily(a.cidr) != ipFamily(b.cidr) {
				continue
			}

			if a.cidr.Contains(b.cidr.IP) || b.cidr.Contains(a.cidr.IP) {
				return fmt.Errorf("overlapping %s between sites: %q %s %s overlaps with %q %s %s", cidrType,
					a.siteName, a.label, a.cidrStr, b.siteName, b.label, b.cidrStr)
			}
		}
	}

	return nil
}

func ipFamily(cidr *net.IPNet) string {
	if cidr.IP.To4() != nil {
		return "ipv4"
	}

	return "ipv6"
}

func assignmentEnabled(enabled *bool) bool {
	if enabled == nil {
		return true
	}

	return *enabled
}

func decodeSiteFromRequest(req *admissionv1.AdmissionRequest) (unboundednetv1alpha1.Site, error) {
	var (
		site unboundednetv1alpha1.Site
		raw  []byte
	)

	if req.Operation == admissionv1.Delete {
		raw = req.OldObject.Raw
	} else {
		raw = req.Object.Raw
	}

	if len(raw) == 0 {
		return site, fmt.Errorf("missing Site object for %s", req.Operation)
	}

	if err := json.Unmarshal(raw, &site); err != nil {
		return site, fmt.Errorf("failed to parse Site: %v", err)
	}

	if site.Name == "" {
		site.Name = req.Name
	}

	return site, nil
}

func decodeGatewayPoolFromRequest(req *admissionv1.AdmissionRequest) (unboundednetv1alpha1.GatewayPool, error) {
	var (
		pool unboundednetv1alpha1.GatewayPool
		raw  []byte
	)

	if req.Operation == admissionv1.Delete {
		raw = req.OldObject.Raw
	} else {
		raw = req.Object.Raw
	}

	if len(raw) == 0 {
		return pool, fmt.Errorf("missing GatewayPool object for %s", req.Operation)
	}

	if err := json.Unmarshal(raw, &pool); err != nil {
		return pool, fmt.Errorf("failed to parse GatewayPool: %v", err)
	}

	if pool.Name == "" {
		pool.Name = req.Name
	}

	return pool, nil
}

func decodeSitePeeringFromRequest(req *admissionv1.AdmissionRequest) (unboundednetv1alpha1.SitePeering, error) {
	var peering unboundednetv1alpha1.SitePeering

	raw := req.Object.Raw
	if len(raw) == 0 {
		return peering, fmt.Errorf("missing SitePeering object for %s", req.Operation)
	}

	if err := json.Unmarshal(raw, &peering); err != nil {
		return peering, fmt.Errorf("failed to parse SitePeering: %v", err)
	}

	if peering.Name == "" {
		peering.Name = req.Name
	}

	return peering, nil
}

func decodeSiteGatewayPoolAssignmentFromRequest(req *admissionv1.AdmissionRequest) (unboundednetv1alpha1.SiteGatewayPoolAssignment, error) {
	var assignment unboundednetv1alpha1.SiteGatewayPoolAssignment

	raw := req.Object.Raw
	if len(raw) == 0 {
		return assignment, fmt.Errorf("missing SiteGatewayPoolAssignment object for %s", req.Operation)
	}

	if err := json.Unmarshal(raw, &assignment); err != nil {
		return assignment, fmt.Errorf("failed to parse SiteGatewayPoolAssignment: %v", err)
	}

	if assignment.Name == "" {
		assignment.Name = req.Name
	}

	return assignment, nil
}

func decodeGatewayPoolPeeringFromRequest(req *admissionv1.AdmissionRequest) (unboundednetv1alpha1.GatewayPoolPeering, error) {
	var peering unboundednetv1alpha1.GatewayPoolPeering

	raw := req.Object.Raw
	if len(raw) == 0 {
		return peering, fmt.Errorf("missing GatewayPoolPeering object for %s", req.Operation)
	}

	if err := json.Unmarshal(raw, &peering); err != nil {
		return peering, fmt.Errorf("failed to parse GatewayPoolPeering: %v", err)
	}

	if peering.Name == "" {
		peering.Name = req.Name
	}

	return peering, nil
}

func mergeSiteList(existing []unboundednetv1alpha1.Site, site unboundednetv1alpha1.Site, op admissionv1.Operation) []unboundednetv1alpha1.Site {
	if op == admissionv1.Delete {
		return existing
	}

	merged := make([]unboundednetv1alpha1.Site, 0, len(existing)+1)
	found := false

	for _, item := range existing {
		if item.Name == site.Name {
			merged = append(merged, site)
			found = true

			continue
		}

		merged = append(merged, item)
	}

	if !found {
		merged = append(merged, site)
	}

	return merged
}

func denyResponse(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: message,
		},
	}
}

func allowResponse(message string) *admissionv1.AdmissionResponse {
	response := &admissionv1.AdmissionResponse{Allowed: true}
	if message != "" {
		response.Result = &metav1.Status{Message: message}
	}

	return response
}

// cidrsOverlap checks whether two CIDR strings overlap.
func cidrsOverlap(a, b string) bool {
	_, aNet, err := net.ParseCIDR(a)
	if err != nil {
		return false
	}

	_, bNet, err := net.ParseCIDR(b)
	if err != nil {
		return false
	}

	return aNet.Contains(bNet.IP) || bNet.Contains(aNet.IP)
}

// validateIntraSiteCIDROverlap checks for overlapping CIDRs within a single
// site's NonMasqueradeCIDRs and LocalCIDRs fields.
func validateIntraSiteCIDROverlap(site unboundednetv1alpha1.Site) error {
	// Check within NonMasqueradeCIDRs
	for i := 0; i < len(site.Spec.NonMasqueradeCIDRs); i++ {
		for j := i + 1; j < len(site.Spec.NonMasqueradeCIDRs); j++ {
			if cidrsOverlap(site.Spec.NonMasqueradeCIDRs[i], site.Spec.NonMasqueradeCIDRs[j]) {
				return fmt.Errorf("spec.nonMasqueradeCIDRs contains overlapping CIDRs: %s and %s",
					site.Spec.NonMasqueradeCIDRs[i], site.Spec.NonMasqueradeCIDRs[j])
			}
		}
	}

	// Check within LocalCIDRs
	for i := 0; i < len(site.Spec.LocalCIDRs); i++ {
		for j := i + 1; j < len(site.Spec.LocalCIDRs); j++ {
			if cidrsOverlap(site.Spec.LocalCIDRs[i], site.Spec.LocalCIDRs[j]) {
				return fmt.Errorf("spec.localCidrs contains overlapping CIDRs: %s and %s",
					site.Spec.LocalCIDRs[i], site.Spec.LocalCIDRs[j])
			}
		}
	}

	// Check NonMasqueradeCIDRs vs LocalCIDRs
	for _, nmCIDR := range site.Spec.NonMasqueradeCIDRs {
		for _, localCIDR := range site.Spec.LocalCIDRs {
			if cidrsOverlap(nmCIDR, localCIDR) {
				return fmt.Errorf("spec.nonMasqueradeCIDRs %s overlaps with spec.localCidrs %s",
					nmCIDR, localCIDR)
			}
		}
	}

	return nil
}
