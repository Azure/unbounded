//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"testing"
	"time"
)

func TestControlViewChecks(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*controlView)
		healthy      bool
		wantProblems int
	}{
		{name: "three distinct seats", healthy: true},
		{
			name: "expired primary",
			mutate: func(v *controlView) {
				v.Primary.RenewTime = v.Now.Add(-time.Minute)
			},
		},
		{
			name: "primary without seat",
			mutate: func(v *controlView) {
				delete(v.Seats, "racer-seat-0")
			},
		},
		{
			name: "expired seat",
			mutate: func(v *controlView) {
				s := v.Seats["racer-seat-2"]
				s.RenewTime = v.Now.Add(-time.Minute)
				v.Seats[s.Name] = s
			},
		},
		{
			name: "replaced pod",
			mutate: func(v *controlView) {
				v.Pods[2].UID = "replacement-uid"
			},
		},
		{
			name: "missing holder UID",
			mutate: func(v *controlView) {
				v.Primary.HolderUID = ""
			},
		},
		{
			name: "holder name must match exactly",
			mutate: func(v *controlView) {
				v.Primary.Holder += "-other"
			},
		},
		{
			name: "holder UID must match exactly",
			mutate: func(v *controlView) {
				v.Primary.HolderUID += "-other"
			},
		},
		{
			name: "unscheduled pod",
			mutate: func(v *controlView) {
				v.Pods[2].Node = ""
			},
		},
		{
			name: "duplicate pod",
			mutate: func(v *controlView) {
				s := v.Seats["racer-seat-1"]
				s.Holder = v.Primary.Holder
				s.HolderUID = v.Primary.HolderUID
				v.Seats[s.Name] = s
			},
			wantProblems: 2,
		},
		{
			name: "different pods on one node",
			mutate: func(v *controlView) {
				v.Pods[1].Node = v.Pods[0].Node
			},
			wantProblems: 1,
		},
		{
			name: "single-node cluster",
			mutate: func(v *controlView) {
				v.Pods = v.Pods[:1]
				delete(v.Seats, "racer-seat-1")
				delete(v.Seats, "racer-seat-2")
			},
			healthy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := controlViewFixture()
			if tt.mutate != nil {
				tt.mutate(&v)
			}

			if got := v.healthy(); got != tt.healthy {
				t.Fatalf("healthy=%t, want %t: %s", got, tt.healthy, v.String())
			}

			if got := len(v.seatProblems()); got != tt.wantProblems {
				t.Fatalf("problems=%v, want %d", v.seatProblems(), tt.wantProblems)
			}
		})
	}
}

func TestLeaseExpiryChecks(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name string
		l    lease
		live bool
	}{
		{name: "valid", l: lease{RenewTime: now, Duration: 30 * time.Second}, live: true},
		{name: "missing renewal", l: lease{Duration: 30 * time.Second}},
		{name: "missing duration", l: lease{RenewTime: now}},
		{name: "negative duration", l: lease{RenewTime: now, Duration: -time.Second}},
		{name: "within clock allowance", l: lease{RenewTime: now.Add(-34 * time.Second), Duration: 30 * time.Second}, live: true},
		{name: "at expiry boundary", l: lease{RenewTime: now.Add(-35 * time.Second), Duration: 30 * time.Second}},
		{name: "expired", l: lease{RenewTime: now.Add(-time.Minute), Duration: 30 * time.Second}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.unexpired(now); got != tt.live {
				t.Fatalf("unexpired=%t, want %t", got, tt.live)
			}
		})
	}
}

func controlViewFixture() controlView {
	v := controlView{
		Now:   time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		Seats: map[string]lease{},
	}

	for i := range seatFloor {
		pod := dataplanePod{
			Name:  fmt.Sprintf("pod-%d", i),
			UID:   fmt.Sprintf("uid-%d", i),
			Node:  fmt.Sprintf("node-%d", i),
			IP:    fmt.Sprintf("10.0.0.%d", i+1),
			Ready: true,
		}
		l := lease{
			Name:      fmt.Sprintf("racer-seat-%d", i),
			Holder:    pod.Name,
			HolderUID: pod.UID,
			RenewTime: v.Now,
			Duration:  30 * time.Second,
		}
		v.Pods = append(v.Pods, pod)
		v.Seats[l.Name] = l
	}

	v.Primary = v.Seats["racer-seat-0"]
	v.Primary.Name = primaryLease

	return v
}
