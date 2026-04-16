// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package allocator

import "testing"

// TestNewAllocator tests new allocator.
func TestNewAllocator(t *testing.T) {
	tests := []struct {
		name         string
		ipv4Pools    []string
		ipv6Pools    []string
		ipv4MaskSize int
		ipv6MaskSize int
		wantErr      bool
	}{
		{
			name:         "valid IPv4 only",
			ipv4Pools:    []string{"10.0.0.0/16"},
			ipv4MaskSize: 24,
			wantErr:      false,
		},
		{
			name:         "valid IPv6 only",
			ipv6Pools:    []string{"fd00::/48"},
			ipv6MaskSize: 64,
			wantErr:      false,
		},
		{
			name:         "valid dual-stack",
			ipv4Pools:    []string{"10.0.0.0/16"},
			ipv6Pools:    []string{"fd00::/48"},
			ipv4MaskSize: 24,
			ipv6MaskSize: 64,
			wantErr:      false,
		},
		{
			name:         "invalid IPv4 mask size - too small",
			ipv4Pools:    []string{"10.0.0.0/24"},
			ipv4MaskSize: 24,
			wantErr:      true,
		},
		{
			name:         "invalid IPv4 mask size - exceeds 32",
			ipv4Pools:    []string{"10.0.0.0/16"},
			ipv4MaskSize: 33,
			wantErr:      true,
		},
		{
			name:         "invalid IPv6 mask size - too small",
			ipv6Pools:    []string{"fd00::/64"},
			ipv6MaskSize: 64,
			wantErr:      true,
		},
		{
			name:         "invalid IPv6 mask size - exceeds 128",
			ipv6Pools:    []string{"fd00::/48"},
			ipv6MaskSize: 129,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipv4Pools, _ := ParseCIDRs(tt.ipv4Pools)
			ipv6Pools, _ := ParseCIDRs(tt.ipv6Pools)

			_, err := NewAllocator(ipv4Pools, ipv6Pools, tt.ipv4MaskSize, tt.ipv6MaskSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewAllocator() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestAllocateIPv4 tests allocate ipv4.
func TestAllocateIPv4(t *testing.T) {
	// Test with /24 from /22 (4 possible /24s)
	ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/22"})

	alloc, err := NewAllocator(ipv4Pools, nil, 24, 0)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	// Allocate all 4 /24s
	expectedCIDRs := []string{
		"10.0.0.0/24",
		"10.0.1.0/24",
		"10.0.2.0/24",
		"10.0.3.0/24",
	}

	for i, expected := range expectedCIDRs {
		cidr, err := alloc.AllocateIPv4()
		if err != nil {
			t.Errorf("Allocation %d failed: %v", i, err)
			continue
		}

		if cidr != expected {
			t.Errorf("Allocation %d: got %s, want %s", i, cidr, expected)
		}
	}

	// Next allocation should fail (pool exhausted)
	_, err = alloc.AllocateIPv4()
	if err != ErrPoolExhausted {
		t.Errorf("Expected ErrPoolExhausted, got %v", err)
	}
}

// TestAllocateIPv6 tests allocate ipv6.
func TestAllocateIPv6(t *testing.T) {
	// Test with /64 from /62 (4 possible /64s)
	ipv6Pools, _ := ParseCIDRs([]string{"fd00::/62"})

	alloc, err := NewAllocator(nil, ipv6Pools, 0, 64)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	// Allocate all 4 /64s
	expectedCIDRs := []string{
		"fd00::/64",
		"fd00:0:0:1::/64",
		"fd00:0:0:2::/64",
		"fd00:0:0:3::/64",
	}

	for i, expected := range expectedCIDRs {
		cidr, err := alloc.AllocateIPv6()
		if err != nil {
			t.Errorf("Allocation %d failed: %v", i, err)
			continue
		}

		if cidr != expected {
			t.Errorf("Allocation %d: got %s, want %s", i, cidr, expected)
		}
	}

	// Next allocation should fail (pool exhausted)
	_, err = alloc.AllocateIPv6()
	if err != ErrPoolExhausted {
		t.Errorf("Expected ErrPoolExhausted, got %v", err)
	}
}

// TestMarkAllocated tests mark allocated.
func TestMarkAllocated(t *testing.T) {
	ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/22"})

	alloc, err := NewAllocator(ipv4Pools, nil, 24, 0)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	// Mark first two CIDRs as already allocated
	alloc.MarkAllocated("10.0.0.0/24")
	alloc.MarkAllocated("10.0.1.0/24")

	// Next allocation should be 10.0.2.0/24
	cidr, err := alloc.AllocateIPv4()
	if err != nil {
		t.Fatalf("Allocation failed: %v", err)
	}

	if cidr != "10.0.2.0/24" {
		t.Errorf("Got %s, want 10.0.2.0/24", cidr)
	}
}

// TestMultiplePools tests multiple pools.
func TestMultiplePools(t *testing.T) {
	// Test with multiple pools
	ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/24", "10.1.0.0/24"})

	alloc, err := NewAllocator(ipv4Pools, nil, 26, 0)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	// Each /24 has 4 /26s, so total 8 allocations
	allocations := make([]string, 0, 8)

	for i := 0; i < 8; i++ {
		cidr, err := alloc.AllocateIPv4()
		if err != nil {
			t.Errorf("Allocation %d failed: %v", i, err)
			break
		}

		allocations = append(allocations, cidr)
	}

	if len(allocations) != 8 {
		t.Errorf("Expected 8 allocations, got %d", len(allocations))
	}

	// Verify first pool CIDRs
	expected := []string{
		"10.0.0.0/26",
		"10.0.0.64/26",
		"10.0.0.128/26",
		"10.0.0.192/26",
		"10.1.0.0/26",
		"10.1.0.64/26",
		"10.1.0.128/26",
		"10.1.0.192/26",
	}

	for i, exp := range expected {
		if allocations[i] != exp {
			t.Errorf("Allocation %d: got %s, want %s", i, allocations[i], exp)
		}
	}

	// Pool should now be exhausted
	_, err = alloc.AllocateIPv4()
	if err != ErrPoolExhausted {
		t.Errorf("Expected ErrPoolExhausted, got %v", err)
	}
}

// TestParseCIDRs tests parse cidrs.
func TestParseCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		cidrs   []string
		wantErr bool
	}{
		{
			name:    "valid IPv4 CIDRs",
			cidrs:   []string{"10.0.0.0/16", "192.168.0.0/24"},
			wantErr: false,
		},
		{
			name:    "valid IPv6 CIDRs",
			cidrs:   []string{"fd00::/48", "2001:db8::/32"},
			wantErr: false,
		},
		{
			name:    "invalid CIDR",
			cidrs:   []string{"invalid"},
			wantErr: true,
		},
		{
			name:    "empty slice",
			cidrs:   []string{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCIDRs(tt.cidrs)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCIDRs() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestIsAllocated tests is allocated.
func TestIsAllocated(t *testing.T) {
	ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/16"})
	alloc, _ := NewAllocator(ipv4Pools, nil, 24, 0)

	cidr := "10.0.5.0/24"

	// Should not be allocated initially
	if alloc.IsAllocated(cidr) {
		t.Error("CIDR should not be allocated initially")
	}

	// Mark as allocated
	alloc.MarkAllocated(cidr)

	// Should now be allocated
	if !alloc.IsAllocated(cidr) {
		t.Error("CIDR should be allocated after MarkAllocated")
	}
}

// TestHasPools tests has pools.
func TestHasPools(t *testing.T) {
	t.Run("IPv4 only", func(t *testing.T) {
		ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/16"})
		alloc, _ := NewAllocator(ipv4Pools, nil, 24, 0)

		if !alloc.HasIPv4Pools() {
			t.Error("Expected HasIPv4Pools to be true")
		}

		if alloc.HasIPv6Pools() {
			t.Error("Expected HasIPv6Pools to be false")
		}
	})

	t.Run("IPv6 only", func(t *testing.T) {
		ipv6Pools, _ := ParseCIDRs([]string{"fd00::/48"})
		alloc, _ := NewAllocator(nil, ipv6Pools, 0, 64)

		if alloc.HasIPv4Pools() {
			t.Error("Expected HasIPv4Pools to be false")
		}

		if !alloc.HasIPv6Pools() {
			t.Error("Expected HasIPv6Pools to be true")
		}
	})

	t.Run("dual-stack", func(t *testing.T) {
		ipv4Pools, _ := ParseCIDRs([]string{"10.0.0.0/16"})
		ipv6Pools, _ := ParseCIDRs([]string{"fd00::/48"})
		alloc, _ := NewAllocator(ipv4Pools, ipv6Pools, 24, 64)

		if !alloc.HasIPv4Pools() {
			t.Error("Expected HasIPv4Pools to be true")
		}

		if !alloc.HasIPv6Pools() {
			t.Error("Expected HasIPv6Pools to be true")
		}
	})
}

// TestNoPoolsConfigured tests no pools configured.
func TestNoPoolsConfigured(t *testing.T) {
	alloc, _ := NewAllocator(nil, nil, 0, 0)

	_, err := alloc.AllocateIPv4()
	if err == nil {
		t.Error("Expected error when no IPv4 pools configured")
	}

	_, err = alloc.AllocateIPv6()
	if err == nil {
		t.Error("Expected error when no IPv6 pools configured")
	}
}
