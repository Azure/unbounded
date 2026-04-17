// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDetectCIDRFamilies tests DetectCIDRFamilies.
func TestDetectCIDRFamilies(t *testing.T) {
	hasIPv4, hasIPv6 := detectCIDRFamilies([]string{"10.0.0.0/24", "bad", "fd00::/64"})
	if !hasIPv4 || !hasIPv6 {
		t.Fatalf("expected both families, got ipv4=%v ipv6=%v", hasIPv4, hasIPv6)
	}

	hasIPv4, hasIPv6 = detectCIDRFamilies([]string{"fd00::/64"})
	if hasIPv4 || !hasIPv6 {
		t.Fatalf("expected ipv6-only, got ipv4=%v ipv6=%v", hasIPv4, hasIPv6)
	}
}

// TestForwardPolicyIsDrop tests ForwardPolicyIsDrop.
func TestForwardPolicyIsDrop(t *testing.T) {
	drop := forwardPolicyIsDrop("iptables", func(binary string) (string, error) {
		return "-P INPUT ACCEPT\n-P FORWARD DROP\n-P OUTPUT ACCEPT\n", nil
	})
	if !drop {
		t.Fatalf("expected FORWARD DROP to be detected")
	}

	accept := forwardPolicyIsDrop("iptables", func(binary string) (string, error) {
		return "-P FORWARD ACCEPT\n", nil
	})
	if accept {
		t.Fatalf("expected FORWARD ACCEPT to not be detected as drop")
	}
}

// TestCollectNodeConfigValidationErrors tests CollectNodeConfigValidationErrors.
func TestCollectNodeConfigValidationErrors(t *testing.T) {
	deps := nodeConfigValidatorDeps{
		readFile: func(path string) ([]byte, error) {
			values := map[string]string{
				"/proc/sys/net/ipv4/ip_forward":           "0\n",
				"/proc/sys/net/ipv6/conf/all/forwarding":  "0\n",
				"/proc/sys/net/ipv4/conf/all/rp_filter":   "1\n",
				"/proc/sys/net/ipv4/conf/eth0/rp_filter":  "2\n",
				"/proc/sys/net/ipv4/conf/cni0/rp_filter":  "1\n",
				"/proc/sys/net/ipv4/conf/lo/rp_filter":    "0\n",
				"/proc/sys/net/ipv4/conf/wg0/rp_filter":   "2\n",
				"/proc/sys/net/ipv4/conf/wg1/rp_filter":   "0\n",
				"/proc/sys/net/ipv4/conf/wg2/rp_filter":   "2\n",
				"/proc/sys/net/ipv4/conf/wg10/rp_filter":  "0\n",
				"/proc/sys/net/ipv4/conf/wg100/rp_filter": "2\n",
			}
			if v, ok := values[path]; ok {
				return []byte(v), nil
			}

			return nil, errors.New("not found")
		},
		glob: func(pattern string) ([]string, error) {
			return []string{
				"/proc/sys/net/ipv4/conf/all/rp_filter",
				"/proc/sys/net/ipv4/conf/eth0/rp_filter",
				"/proc/sys/net/ipv4/conf/cni0/rp_filter",
			}, nil
		},
		runForwardPolicy: func(binary string) (string, error) {
			if binary == "iptables" {
				return "-P FORWARD DROP\n", nil
			}

			if binary == "ip6tables" {
				return "-P FORWARD DROP\n", nil
			}

			return "", nil
		},
	}

	problems := collectNodeConfigValidationErrors([]string{"10.42.0.0/16", "fd00::/64"}, deps)
	if len(problems) != 5 {
		t.Fatalf("expected all checks to fire, got %#v", problems)
	}

	types := make([]string, 0, len(problems))
	for _, problem := range problems {
		types = append(types, problem.Type)
	}

	for _, wantType := range []string{
		nodeErrorTypeConfigIPv4ForwardingDisabled,
		nodeErrorTypeConfigIPv6ForwardingDisabled,
		nodeErrorTypeConfigIptablesForwardDrop,
		nodeErrorTypeConfigIp6tablesForwardDrop,
		nodeErrorTypeConfigRPFilterStrict,
	} {
		if !slices.Contains(types, wantType) {
			t.Fatalf("expected problem type %q in %#v", wantType, types)
		}
	}
}

// TestCollectNodeConfigValidationErrors_NoIPv6CIDRSkipsIPv6Forwarding tests IPv6 forwarding gating.
func TestCollectNodeConfigValidationErrors_NoIPv6CIDRSkipsIPv6Forwarding(t *testing.T) {
	deps := nodeConfigValidatorDeps{
		readFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/sys/net/ipv4/ip_forward":
				return []byte("1\n"), nil
			case "/proc/sys/net/ipv6/conf/all/forwarding":
				return []byte("0\n"), nil
			default:
				return []byte("0\n"), nil
			}
		},
		glob: func(pattern string) ([]string, error) { return nil, nil },
		runForwardPolicy: func(binary string) (string, error) {
			return "-P FORWARD ACCEPT\n", nil
		},
	}

	problems := collectNodeConfigValidationErrors([]string{"10.42.0.0/16"}, deps)
	for _, problem := range problems {
		if problem.Type == nodeErrorTypeConfigIPv6ForwardingDisabled {
			t.Fatalf("unexpected ipv6 forwarding problem when local site has no ipv6 pod CIDRs: %#v", problems)
		}
	}
}

// TestFindStrictRPFilterInterfaces tests FindStrictRPFilterInterfaces.
func TestFindStrictRPFilterInterfaces(t *testing.T) {
	deps := nodeConfigValidatorDeps{
		readFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "/eth0/") {
				return []byte("1\n"), nil
			}

			if strings.Contains(path, "/all/") {
				return []byte("1\n"), nil
			}

			if strings.Contains(path, "/default/") {
				return []byte("1\n"), nil
			}

			if strings.Contains(path, "/lo/") {
				return []byte("1\n"), nil
			}

			return []byte("2\n"), nil
		},
		glob: func(pattern string) ([]string, error) {
			return []string{
				"/proc/sys/net/ipv4/conf/eth0/rp_filter",
				"/proc/sys/net/ipv4/conf/all/rp_filter",
				"/proc/sys/net/ipv4/conf/default/rp_filter",
				"/proc/sys/net/ipv4/conf/lo/rp_filter",
				"/proc/sys/net/ipv4/conf/cni0/rp_filter",
			}, nil
		},
	}

	got := findStrictRPFilterInterfaces(deps)

	want := []string{"eth0"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected strict rp_filter interfaces: got %#v want %#v", got, want)
	}
}

// TestShouldLogNodeConfigProblems tests ShouldLogNodeConfigProblems.
func TestShouldLogNodeConfigProblems(t *testing.T) {
	now := time.Now()
	interval := 30 * time.Second

	if !shouldLogNodeConfigProblems(time.Time{}, "", "a=b", interval, now) {
		t.Fatalf("expected first problem observation to log")
	}

	if !shouldLogNodeConfigProblems(now, "a=b", "c=d", interval, now.Add(5*time.Second)) {
		t.Fatalf("expected signature change to log immediately")
	}

	if shouldLogNodeConfigProblems(now, "a=b", "a=b", interval, now.Add(5*time.Second)) {
		t.Fatalf("expected unchanged signature before interval to skip logging")
	}

	if !shouldLogNodeConfigProblems(now, "a=b", "a=b", interval, now.Add(31*time.Second)) {
		t.Fatalf("expected unchanged signature after interval to log")
	}
}

// TestCheckConntrackUsage tests conntrack threshold detection.
func TestCheckConntrackUsage(t *testing.T) {
	readFile := func(files map[string]string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if v, ok := files[path]; ok {
				return []byte(v), nil
			}

			return nil, errors.New("not found")
		}
	}

	// Usage below threshold (50%) -- no error.
	err := checkConntrackUsage(readFile(map[string]string{
		"/proc/sys/net/netfilter/nf_conntrack_count": "50000",
		"/proc/sys/net/netfilter/nf_conntrack_max":   "100000",
	}))
	if err != nil {
		t.Fatalf("expected no error at 50%%, got: %v", err)
	}

	// Usage at exactly 80% -- should warn.
	err = checkConntrackUsage(readFile(map[string]string{
		"/proc/sys/net/netfilter/nf_conntrack_count": "80000",
		"/proc/sys/net/netfilter/nf_conntrack_max":   "100000",
	}))
	if err == nil {
		t.Fatalf("expected error at 80%%, got nil")
	}

	if !strings.Contains(err.Error(), "80%") {
		t.Fatalf("expected 80%% in message, got: %v", err)
	}

	// Usage at 95% -- should warn.
	err = checkConntrackUsage(readFile(map[string]string{
		"/proc/sys/net/netfilter/nf_conntrack_count": "95000",
		"/proc/sys/net/netfilter/nf_conntrack_max":   "100000",
	}))
	if err == nil {
		t.Fatalf("expected error at 95%%, got nil")
	}

	// Files missing -- no error (graceful degradation).
	err = checkConntrackUsage(readFile(map[string]string{}))
	if err != nil {
		t.Fatalf("expected no error when files missing, got: %v", err)
	}

	// Max is 0 -- no error (avoid division by zero).
	err = checkConntrackUsage(readFile(map[string]string{
		"/proc/sys/net/netfilter/nf_conntrack_count": "100",
		"/proc/sys/net/netfilter/nf_conntrack_max":   "0",
	}))
	if err != nil {
		t.Fatalf("expected no error when max=0, got: %v", err)
	}
}

// TestReadProcInt tests the readProcInt helper.
func TestReadProcInt(t *testing.T) {
	read := func(path string) ([]byte, error) {
		if path == "/proc/test" {
			return []byte("  42\n"), nil
		}

		return nil, errors.New("missing")
	}

	val, err := readProcInt("/proc/test", read)
	if err != nil || val != 42 {
		t.Fatalf("expected 42, got %d err=%v", val, err)
	}

	_, err = readProcInt("/proc/missing", read)
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}
