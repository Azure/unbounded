// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ipallocator resolves incomplete DHCP leases on Machine resources.
//
// When a DHCPLease entry specifies only a MAC address (omitting IPv4,
// SubnetMask, and Gateway), the reconciler looks up the unbounded-net
// Site object matching the Machine's site label. It derives the subnet
// mask and gateway from the Site's nodeCidrs, picks a random IP that is
// not already assigned to any other Machine, and patches the lease fields
// onto the Machine spec.
//
// Because the allocated address is written directly to the spec, all
// downstream consumers (DHCP server, HTTP server, field indexes) work
// without modification.
package ipallocator

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
)

const siteLabel = "unbounded-kube.io/site"

var siteGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sites",
}

// Reconciler watches Machine resources and fills in incomplete DHCP
// leases from the matching unbounded-net Site object.
type Reconciler struct {
	Client    client.Client
	APIReader client.Reader
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Build an unstructured object to watch Site resources.
	siteObj := &unstructured.Unstructured{}
	siteObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   siteGVR.Group,
		Version: siteGVR.Version,
		Kind:    "Site",
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("ipallocator").
		For(&v1alpha3.Machine{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return needsAllocation(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return needsAllocation(e.ObjectNew)
			},
			DeleteFunc: func(_ event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return needsAllocation(e.Object)
			},
		}).
		Watches(siteObj, handler.EnqueueRequestsFromMapFunc(r.machinesForSite)).
		Complete(r)
}

// machinesForSite maps a Site event to all Machines that reference
// that Site via the unbounded-kube.io/site label and have incomplete
// DHCP leases.
func (r *Reconciler) machinesForSite(ctx context.Context, obj client.Object) []reconcile.Request {
	var machines v1alpha3.MachineList
	if err := r.Client.List(ctx, &machines, client.MatchingLabels{siteLabel: obj.GetName()}); err != nil {
		slog.Error("ipallocator: listing machines for site", "site", obj.GetName(), "err", err)
		return nil
	}

	var requests []reconcile.Request
	for _, m := range machines.Items {
		if needsAllocation(&m) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&m),
			})
		}
	}

	return requests
}

// needsAllocation returns true if the Machine has at least one DHCP
// lease that is missing IP configuration.
func needsAllocation(obj client.Object) bool {
	m, ok := obj.(*v1alpha3.Machine)
	if !ok || m.Spec.PXE == nil {
		return false
	}

	for _, lease := range m.Spec.PXE.DHCPLeases {
		if leaseNeedsAllocation(&lease) {
			return true
		}
	}

	return false
}

// leaseNeedsAllocation returns true when the lease is missing any of
// the IP configuration fields that should be auto-allocated.
func leaseNeedsAllocation(lease *v1alpha3.DHCPLease) bool {
	return lease.IPv4 == "" || lease.SubnetMask == "" || lease.Gateway == ""
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("reconciler", "ipallocator", "machine", req.Name)

	var machine v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if machine.Spec.PXE == nil {
		return ctrl.Result{}, nil
	}

	// Check whether any lease needs allocation.
	needsWork := false

	for i := range machine.Spec.PXE.DHCPLeases {
		if leaseNeedsAllocation(&machine.Spec.PXE.DHCPLeases[i]) {
			needsWork = true
			break
		}
	}

	if !needsWork {
		return ctrl.Result{}, nil
	}

	// Look up the Site for this Machine.
	siteName := machine.Labels[siteLabel]
	if siteName == "" {
		log.Warn("machine has incomplete DHCP leases but no site label; cannot auto-allocate")
		return ctrl.Result{}, nil
	}

	siteInfo, err := r.fetchSiteInfo(ctx, siteName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching site %q: %w", siteName, err)
	}

	if len(siteInfo.subnets) == 0 {
		log.Warn("site has no nodeCidrs; cannot auto-allocate", "site", siteName)
		return ctrl.Result{}, nil
	}

	// Collect all IPs already allocated to any Machine.
	allocated, err := r.allocatedIPs(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("collecting allocated IPs: %w", err)
	}

	// Fill in incomplete leases.
	changed := false

	for i := range machine.Spec.PXE.DHCPLeases {
		lease := &machine.Spec.PXE.DHCPLeases[i]
		if !leaseNeedsAllocation(lease) {
			continue
		}

		// Use the first subnet from the Site that is large enough.
		subnet := siteInfo.subnets[0]

		if lease.SubnetMask == "" {
			lease.SubnetMask = subnetMaskString(subnet)
			changed = true
		}

		if lease.Gateway == "" {
			lease.Gateway = firstUsableIP(subnet).String()
			changed = true
		}

		if lease.IPv4 == "" {
			var chosen net.IP
			for attempt := uint32(0); attempt < subnetSize(subnet); attempt++ {
				candidate, err := randomAvailableIP(subnet, allocated)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("allocating IP for MAC %s: %w", lease.MAC, err)
				}

				// Verify against the live index to guard against races
				// with other concurrent reconciles.
				var existing v1alpha3.MachineList
				if err := r.Client.List(ctx, &existing, client.MatchingFields{indexing.IndexNodeByIP: candidate.String()}); err != nil {
					return ctrl.Result{}, fmt.Errorf("checking IP index for %s: %w", candidate, err)
				}

				if len(existing.Items) == 0 {
					chosen = candidate
					break
				}

				// IP is taken; mark it and try again.
				allocated[candidate.String()] = true
			}

			if chosen == nil {
				return ctrl.Result{}, fmt.Errorf("no available addresses in subnet %s for MAC %s", subnet, lease.MAC)
			}

			lease.IPv4 = chosen.String()
			allocated[chosen.String()] = true
			changed = true

			log.Info("allocated IP for DHCP lease",
				"mac", lease.MAC, "ipv4", lease.IPv4,
				"subnetMask", lease.SubnetMask, "gateway", lease.Gateway)
		}
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	if err := r.Client.Update(ctx, &machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating Machine spec: %w", err)
	}

	return ctrl.Result{}, nil
}

// siteInfo holds the parsed subnet information from a Site object.
type siteInfo struct {
	subnets []*net.IPNet
}

// fetchSiteInfo reads the unbounded-net Site object and parses its nodeCidrs.
func (r *Reconciler) fetchSiteInfo(ctx context.Context, name string) (*siteInfo, error) {
	var site unstructured.Unstructured

	site.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   siteGVR.Group,
		Version: siteGVR.Version,
		Kind:    "Site",
	})

	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, &site); err != nil {
		return nil, fmt.Errorf("getting Site %q: %w", name, err)
	}

	spec, ok := site.Object["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("site %q has no spec", name)
	}

	nodeCidrsRaw, ok := spec["nodeCidrs"].([]interface{})
	if !ok || len(nodeCidrsRaw) == 0 {
		return nil, fmt.Errorf("site %q has no nodeCidrs", name)
	}

	info := &siteInfo{}

	for _, raw := range nodeCidrsRaw {
		cidr, ok := raw.(string)
		if !ok {
			continue
		}

		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("parsing CIDR %q: %w", cidr, err)
		}

		// Only use IPv4 subnets for DHCP lease allocation.
		if ipNet.IP.To4() != nil {
			info.subnets = append(info.subnets, ipNet)
		}
	}

	return info, nil
}

// allocatedIPs returns the set of all IPv4 addresses currently assigned
// to any Machine's DHCP leases.
func (r *Reconciler) allocatedIPs(ctx context.Context) (map[string]bool, error) {
	var list v1alpha3.MachineList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing Machines: %w", err)
	}

	ips := make(map[string]bool)

	for _, m := range list.Items {
		if m.Spec.PXE == nil {
			continue
		}

		for _, lease := range m.Spec.PXE.DHCPLeases {
			if lease.IPv4 != "" {
				ips[lease.IPv4] = true
			}
		}
	}

	return ips, nil
}

// subnetMaskString returns the subnet mask as a dotted-decimal string.
func subnetMaskString(n *net.IPNet) string {
	mask := n.Mask

	// Ensure we have a 4-byte mask for IPv4.
	if len(mask) == 16 {
		mask = mask[12:]
	}

	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}

// firstUsableIP returns the first usable host address in the subnet
// (network address + 1). This is conventionally the gateway.
func firstUsableIP(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	if ip == nil {
		return nil
	}

	result := make(net.IP, 4)
	copy(result, ip)
	result[3]++

	return result
}

// lastUsableIP returns the last usable host address in the subnet
// (broadcast address - 1).
func lastUsableIP(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	if ip == nil {
		return nil
	}

	mask := n.Mask
	if len(mask) == 16 {
		mask = mask[12:]
	}

	bcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bcast[i] = ip[i] | ^mask[i]
	}

	bcast[3]--

	return bcast
}

// subnetSize returns the number of usable host addresses in the subnet
// (excluding network and broadcast addresses, and the gateway at .1).
func subnetSize(n *net.IPNet) uint32 {
	ones, bits := n.Mask.Size()
	total := uint32(1) << uint(bits-ones)

	if total <= 3 {
		return 0 // Network, broadcast, and gateway consume all addresses.
	}

	return total - 3 // Subtract network, broadcast, and gateway (.1).
}

// RandomAvailableIP picks a random usable IPv4 address from the subnet
// that is not in the allocated set. It starts at a random offset and
// scans forward to avoid birthday-collision retries on sparse subnets.
//
// Exported for testing.
func RandomAvailableIP(subnet *net.IPNet, allocated map[string]bool) (net.IP, error) {
	return randomAvailableIP(subnet, allocated)
}

func randomAvailableIP(subnet *net.IPNet, allocated map[string]bool) (net.IP, error) {
	size := subnetSize(subnet)
	if size == 0 {
		return nil, fmt.Errorf("subnet %s has no usable addresses", subnet)
	}

	// The usable host range starts at firstUsable+1 (skip gateway at .1)
	// and ends at lastUsable (broadcast - 1). We number these 0..size-1.
	first := firstUsableIP(subnet)
	startNum := ipToUint32(first) + 1 // Skip gateway.

	offset := rand.Uint32N(size) //nolint:gosec // Cryptographic randomness not needed for IP selection.

	for i := uint32(0); i < size; i++ {
		candidate := uint32ToIP((startNum + ((offset + i) % size)))
		if !allocated[candidate.String()] {
			return candidate, nil
		}
	}

	return nil, fmt.Errorf("no available addresses in subnet %s", subnet)
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return binary.BigEndian.Uint32(ip)
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)

	return ip
}
