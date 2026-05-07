// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package allocator provides CIDR allocation functionality for Kubernetes nodes.
package allocator

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"sort"
	"sync"

	"k8s.io/klog/v2"
)

var (
	// ErrPoolExhausted is returned when no more CIDRs are available for allocation.
	ErrPoolExhausted = errors.New("CIDR pool exhausted")
	// ErrInvalidMaskSize is returned when the mask size is invalid for the given pool.
	ErrInvalidMaskSize = errors.New("invalid mask size for pool")
)

// Allocator manages CIDR allocation from configured pools.
type Allocator struct {
	mu           sync.Mutex
	ipv4Pools    []*net.IPNet
	ipv6Pools    []*net.IPNet
	ipv4MaskSize int
	ipv6MaskSize int
	allocated    map[string]bool
}

// NewAllocator creates a new CIDR allocator with the given pools and mask sizes.
// Returns an error if the mask sizes are invalid for the given pools.
func NewAllocator(ipv4Pools, ipv6Pools []*net.IPNet, ipv4MaskSize, ipv6MaskSize int) (*Allocator, error) {
	klog.V(2).Infof("Creating new allocator with %d IPv4 pools and %d IPv6 pools", len(ipv4Pools), len(ipv6Pools))
	klog.V(4).Infof("IPv4 mask size: /%d, IPv6 mask size: /%d", ipv4MaskSize, ipv6MaskSize)

	a := &Allocator{
		ipv4Pools:    ipv4Pools,
		ipv6Pools:    ipv6Pools,
		ipv4MaskSize: ipv4MaskSize,
		ipv6MaskSize: ipv6MaskSize,
		allocated:    make(map[string]bool),
	}

	// Validate IPv4 mask size
	for i, pool := range ipv4Pools {
		ones, bits := pool.Mask.Size()
		klog.V(4).Infof("Validating IPv4 pool[%d]: %s (prefix /%d, bits %d)", i, pool.String(), ones, bits)

		if ipv4MaskSize <= ones {
			klog.Errorf("IPv4 validation failed: mask size %d must be larger than pool prefix /%d", ipv4MaskSize, ones)
			return nil, fmt.Errorf("%w: IPv4 mask size %d must be larger than pool prefix /%d", ErrInvalidMaskSize, ipv4MaskSize, ones)
		}

		if ipv4MaskSize > 32 {
			klog.Errorf("IPv4 validation failed: mask size %d exceeds maximum of 32", ipv4MaskSize)
			return nil, fmt.Errorf("%w: IPv4 mask size %d exceeds maximum of 32", ErrInvalidMaskSize, ipv4MaskSize)
		}

		subnetBits := ipv4MaskSize - ones
		maxSubnets := big.NewInt(1)
		maxSubnets.Lsh(maxSubnets, uint(subnetBits))
		klog.V(2).Infof("IPv4 pool[%d] %s: can allocate up to %s subnets of /%d", i, pool.String(), maxSubnets.String(), ipv4MaskSize)
	}

	// Validate IPv6 mask size
	for i, pool := range ipv6Pools {
		ones, bits := pool.Mask.Size()
		klog.V(4).Infof("Validating IPv6 pool[%d]: %s (prefix /%d, bits %d)", i, pool.String(), ones, bits)

		if ipv6MaskSize <= ones {
			klog.Errorf("IPv6 validation failed: mask size %d must be larger than pool prefix /%d", ipv6MaskSize, ones)
			return nil, fmt.Errorf("%w: IPv6 mask size %d must be larger than pool prefix /%d", ErrInvalidMaskSize, ipv6MaskSize, ones)
		}

		if ipv6MaskSize > 128 {
			klog.Errorf("IPv6 validation failed: mask size %d exceeds maximum of 128", ipv6MaskSize)
			return nil, fmt.Errorf("%w: IPv6 mask size %d exceeds maximum of 128", ErrInvalidMaskSize, ipv6MaskSize)
		}

		subnetBits := ipv6MaskSize - ones
		maxSubnets := big.NewInt(1)
		maxSubnets.Lsh(maxSubnets, uint(subnetBits))
		klog.V(2).Infof("IPv6 pool[%d] %s: can allocate up to %s subnets of /%d", i, pool.String(), maxSubnets.String(), ipv6MaskSize)
	}

	klog.V(2).Info("Allocator created successfully")

	return a, nil
}

// MarkAllocated marks a CIDR as already allocated.
// This should be called for all existing node podCIDRs during startup.
func (a *Allocator) MarkAllocated(cidr string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.allocated[cidr] {
		klog.V(4).Infof("CIDR %s already marked as allocated, skipping", cidr)
		return
	}

	a.allocated[cidr] = true
	klog.V(3).Infof("Marked CIDR %s as allocated (total allocated: %d)", cidr, len(a.allocated))
}

// Release marks a CIDR as no longer allocated, freeing it for future use.
// This should be called when a node is deleted.
func (a *Allocator) Release(cidr string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.allocated[cidr] {
		klog.V(4).Infof("CIDR %s was not allocated, nothing to release", cidr)
		return
	}

	delete(a.allocated, cidr)
	klog.Infof("Released CIDR %s (total allocated: %d)", cidr, len(a.allocated))
}

// Reset clears all allocation state. This should be called before re-initializing
// the allocator when becoming leader to ensure we have a fresh state.
func (a *Allocator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	count := len(a.allocated)
	a.allocated = make(map[string]bool)

	klog.Infof("Reset allocator state (cleared %d allocations)", count)
}

// IsAllocated checks if a CIDR is already allocated.
func (a *Allocator) IsAllocated(cidr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	allocated := a.allocated[cidr]
	klog.V(5).Infof("IsAllocated check for %s: %v", cidr, allocated)

	return allocated
}

// ContainsCIDR returns true if the given CIDR falls within any of the allocator's pools.
func (a *Allocator) ContainsCIDR(cidr string) bool {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}

	ip := ipNet.IP
	for _, pool := range a.ipv4Pools {
		if pool.Contains(ip) {
			return true
		}
	}

	for _, pool := range a.ipv6Pools {
		if pool.Contains(ip) {
			return true
		}
	}

	return false
}

// AllocateIPv4 allocates the next available IPv4 CIDR from the pools.
// Returns ErrPoolExhausted if no CIDRs are available.
func (a *Allocator) AllocateIPv4() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	klog.V(3).Info("AllocateIPv4: starting allocation attempt")
	klog.V(4).Infof("AllocateIPv4: current allocation count: %d", len(a.allocated))

	if len(a.ipv4Pools) == 0 {
		klog.Error("AllocateIPv4: no IPv4 pools configured")
		return "", fmt.Errorf("no IPv4 pools configured")
	}

	for i, pool := range a.ipv4Pools {
		klog.V(3).Infof("AllocateIPv4: trying pool[%d] %s", i, pool.String())

		cidr, err := a.allocateFromPool(pool, a.ipv4MaskSize)
		if err == nil {
			klog.V(3).Infof("AllocateIPv4: successfully allocated %s from pool[%d] %s", cidr, i, pool.String())
			return cidr, nil
		}

		if !errors.Is(err, ErrPoolExhausted) {
			klog.Errorf("AllocateIPv4: unexpected error from pool[%d] %s: %v", i, pool.String(), err)
			return "", err
		}

		klog.V(3).Infof("AllocateIPv4: pool[%d] %s exhausted, trying next pool", i, pool.String())
	}

	klog.Errorf("AllocateIPv4: all %d pools exhausted", len(a.ipv4Pools))

	return "", ErrPoolExhausted
}

// AllocateIPv6 allocates the next available IPv6 CIDR from the pools.
// Returns ErrPoolExhausted if no CIDRs are available.
func (a *Allocator) AllocateIPv6() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	klog.V(3).Info("AllocateIPv6: starting allocation attempt")
	klog.V(4).Infof("AllocateIPv6: current allocation count: %d", len(a.allocated))

	if len(a.ipv6Pools) == 0 {
		klog.Error("AllocateIPv6: no IPv6 pools configured")
		return "", fmt.Errorf("no IPv6 pools configured")
	}

	for i, pool := range a.ipv6Pools {
		klog.V(3).Infof("AllocateIPv6: trying pool[%d] %s", i, pool.String())

		cidr, err := a.allocateFromPool(pool, a.ipv6MaskSize)
		if err == nil {
			klog.V(3).Infof("AllocateIPv6: successfully allocated %s from pool[%d] %s", cidr, i, pool.String())
			return cidr, nil
		}

		if !errors.Is(err, ErrPoolExhausted) {
			klog.Errorf("AllocateIPv6: unexpected error from pool[%d] %s: %v", i, pool.String(), err)
			return "", err
		}

		klog.V(3).Infof("AllocateIPv6: pool[%d] %s exhausted, trying next pool", i, pool.String())
	}

	klog.Errorf("AllocateIPv6: all %d pools exhausted", len(a.ipv6Pools))

	return "", ErrPoolExhausted
}

// allocateFromPool finds and allocates the next available CIDR from a single pool.
// Must be called with the mutex held.
func (a *Allocator) allocateFromPool(pool *net.IPNet, maskSize int) (string, error) {
	poolOnes, poolBits := pool.Mask.Size()
	klog.V(4).Infof("allocateFromPool: pool=%s, poolOnes=%d, poolBits=%d, targetMaskSize=%d", pool.String(), poolOnes, poolBits, maskSize)

	// Calculate how many subnets we can create
	subnetBits := maskSize - poolOnes
	if subnetBits <= 0 {
		klog.Errorf("allocateFromPool: invalid subnet bits %d (maskSize=%d, poolOnes=%d)", subnetBits, maskSize, poolOnes)
		return "", fmt.Errorf("%w: mask size %d must be larger than pool prefix /%d", ErrInvalidMaskSize, maskSize, poolOnes)
	}

	// Create the subnet mask
	subnetMask := net.CIDRMask(maskSize, poolBits)

	// Iterate through all possible subnets
	numSubnets := big.NewInt(1)
	numSubnets.Lsh(numSubnets, uint(subnetBits))
	klog.V(4).Infof("allocateFromPool: pool %s has %s possible subnets of /%d", pool.String(), numSubnets.String(), maskSize)

	baseIP := pool.IP
	checkedCount := int64(0)
	skippedCount := int64(0)

	for i := big.NewInt(0); i.Cmp(numSubnets) < 0; i.Add(i, big.NewInt(1)) {
		subnetIP := addIPOffset(baseIP, i, maskSize, poolBits)
		subnet := &net.IPNet{IP: subnetIP, Mask: subnetMask}
		cidrStr := subnet.String()
		checkedCount++

		if a.allocated[cidrStr] {
			skippedCount++
			if skippedCount <= 10 || skippedCount%100 == 0 {
				klog.V(5).Infof("allocateFromPool: CIDR %s already allocated (skipped %d so far)", cidrStr, skippedCount)
			}

			continue
		}

		a.allocated[cidrStr] = true
		klog.V(3).Infof("allocateFromPool: allocated %s (checked %d, skipped %d already allocated)", cidrStr, checkedCount, skippedCount)

		return cidrStr, nil
	}

	klog.V(3).Infof("allocateFromPool: pool %s exhausted after checking %d subnets (all %d allocated)", pool.String(), checkedCount, skippedCount)

	return "", ErrPoolExhausted
}

// addIPOffset adds an offset to a base IP address to calculate a subnet address.
func addIPOffset(baseIP net.IP, index *big.Int, maskSize, totalBits int) net.IP {
	// Normalize the IP to the correct length for the address family
	var normalizedIP net.IP
	if totalBits == 32 {
		normalizedIP = baseIP.To4()
		if normalizedIP == nil {
			klog.Errorf("addIPOffset: failed to convert baseIP %s to IPv4", baseIP.String())
			return nil
		}
	} else {
		normalizedIP = baseIP.To16()
		if normalizedIP == nil {
			klog.Errorf("addIPOffset: failed to convert baseIP %s to IPv6", baseIP.String())
			return nil
		}
	}

	// Convert base IP to big.Int
	ipInt := big.NewInt(0)
	ipInt.SetBytes(normalizedIP)

	// Calculate the bit shift needed (offset is in terms of subnet blocks)
	shift := uint(totalBits - maskSize)

	// Calculate the offset
	offset := big.NewInt(0)
	offset.Lsh(index, shift)

	// Add offset to base IP
	result := big.NewInt(0)
	result.Add(ipInt, offset)

	klog.V(5).Infof("addIPOffset: baseIP=%s, normalizedIP=%v, index=%s, shift=%d, offset=%s, result=%s",
		baseIP.String(), normalizedIP, index.String(), shift, offset.String(), result.String())

	// Convert back to IP
	resultBytes := result.Bytes()
	ipLen := totalBits / 8

	// Pad with leading zeros if necessary
	if len(resultBytes) < ipLen {
		padded := make([]byte, ipLen)
		copy(padded[ipLen-len(resultBytes):], resultBytes)
		resultBytes = padded
	}

	// Truncate if too long (shouldn't happen, but be safe)
	if len(resultBytes) > ipLen {
		resultBytes = resultBytes[len(resultBytes)-ipLen:]
	}

	klog.V(5).Infof("addIPOffset: resultBytes=%v (len=%d, expected=%d)", resultBytes, len(resultBytes), ipLen)

	if totalBits == 32 {
		return net.IP(resultBytes).To4()
	}

	return net.IP(resultBytes).To16()
}

// HasIPv4Pools returns true if IPv4 pools are configured.
func (a *Allocator) HasIPv4Pools() bool {
	has := len(a.ipv4Pools) > 0
	klog.V(5).Infof("HasIPv4Pools: %v (count: %d)", has, len(a.ipv4Pools))

	return has
}

// HasIPv6Pools returns true if IPv6 pools are configured.
func (a *Allocator) HasIPv6Pools() bool {
	has := len(a.ipv6Pools) > 0
	klog.V(5).Infof("HasIPv6Pools: %v (count: %d)", has, len(a.ipv6Pools))

	return has
}

// AllocatorDebugState contains a snapshot of allocator internal state.
type AllocatorDebugState struct {
	IPv4Pools      []string `json:"ipv4Pools"`
	IPv6Pools      []string `json:"ipv6Pools"`
	IPv4MaskSize   int      `json:"ipv4MaskSize"`
	IPv6MaskSize   int      `json:"ipv6MaskSize"`
	AllocatedCount int      `json:"allocatedCount"`
	AllocatedCIDRs []string `json:"allocatedCIDRs"`
}

// DebugState returns a snapshot of the allocator's internal state.
func (a *Allocator) DebugState() AllocatorDebugState {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := AllocatorDebugState{
		IPv4MaskSize:   a.ipv4MaskSize,
		IPv6MaskSize:   a.ipv6MaskSize,
		AllocatedCount: len(a.allocated),
	}
	for _, p := range a.ipv4Pools {
		state.IPv4Pools = append(state.IPv4Pools, p.String())
	}

	for _, p := range a.ipv6Pools {
		state.IPv6Pools = append(state.IPv6Pools, p.String())
	}

	state.AllocatedCIDRs = make([]string, 0, len(a.allocated))
	for cidr := range a.allocated {
		state.AllocatedCIDRs = append(state.AllocatedCIDRs, cidr)
	}

	sort.Strings(state.AllocatedCIDRs)

	return state
}

// ParseCIDRs parses a slice of CIDR strings into net.IPNet objects.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	klog.V(3).Infof("ParseCIDRs: parsing %d CIDR strings", len(cidrs))

	result := make([]*net.IPNet, 0, len(cidrs))
	for i, cidr := range cidrs {
		klog.V(4).Infof("ParseCIDRs: parsing CIDR[%d]: %q", i, cidr)

		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			klog.Errorf("ParseCIDRs: failed to parse CIDR[%d] %q: %v", i, cidr, err)
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}

		klog.V(4).Infof("ParseCIDRs: successfully parsed CIDR[%d]: %s", i, ipNet.String())
		result = append(result, ipNet)
	}

	klog.V(3).Infof("ParseCIDRs: successfully parsed %d CIDRs", len(result))

	return result, nil
}
