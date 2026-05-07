// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	nodeErrorTypeConfigIPv4ForwardingDisabled = "configIPv4ForwardingDisabled"
	nodeErrorTypeConfigIPv6ForwardingDisabled = "configIPv6ForwardingDisabled"
	nodeErrorTypeConfigIptablesForwardDrop    = "configIptablesForwardDrop"
	nodeErrorTypeConfigIp6tablesForwardDrop   = "configIp6tablesForwardDrop"
	nodeErrorTypeConfigRPFilterStrict         = "configRPFilterStrict"
	nodeErrorTypeConntrackNearMax             = "conntrackNearMax"

	// conntrackUsageThreshold is the fraction of nf_conntrack_max at which a
	// warning is raised. 0.80 means "warn when usage >= 80% of max".
	conntrackUsageThreshold = 0.80
)

type nodeConfigValidatorDeps struct {
	readFile         func(path string) ([]byte, error)
	glob             func(pattern string) ([]string, error)
	runForwardPolicy func(binary string) (string, error)
}

var defaultNodeConfigValidatorDeps = nodeConfigValidatorDeps{
	readFile: os.ReadFile,
	glob:     filepath.Glob,
	runForwardPolicy: func(binary string) (string, error) {
		out, err := exec.Command(binary, "-S", "FORWARD").CombinedOutput()
		return string(out), err
	},
}

// refreshNodeConfigurationProblems evaluates runtime forwarding configuration, updates nodeErrors, and logs persistent warnings.
func refreshNodeConfigurationProblems(siteInformer cache.SharedIndexInformer, siteName string, state *wireGuardState, logInterval time.Duration) {
	if state == nil {
		return
	}

	state.mu.Lock()
	nodePodCIDRs := append([]string(nil), state.nodePodCIDRs...)
	state.mu.Unlock()

	sitePodCIDRs := getLocalSitePodCIDRs(siteInformer, siteName, nodePodCIDRs)
	configProblems := collectNodeConfigValidationErrors(sitePodCIDRs, defaultNodeConfigValidatorDeps)

	configProblemTypes := []string{
		nodeErrorTypeConfigIPv4ForwardingDisabled,
		nodeErrorTypeConfigIPv6ForwardingDisabled,
		nodeErrorTypeConfigIptablesForwardDrop,
		nodeErrorTypeConfigIp6tablesForwardDrop,
		nodeErrorTypeConfigRPFilterStrict,
		nodeErrorTypeConntrackNearMax,
	}

	configProblemTypeSet := make(map[string]struct{}, len(configProblemTypes))
	for _, problemType := range configProblemTypes {
		configProblemTypeSet[problemType] = struct{}{}
	}

	state.mu.Lock()

	filtered := make([]NodeError, 0, len(state.nodeErrors)+len(configProblems))
	for _, nodeErr := range state.nodeErrors {
		if _, isConfigProblem := configProblemTypeSet[nodeErr.Type]; isConfigProblem {
			continue
		}

		filtered = append(filtered, nodeErr)
	}

	state.nodeErrors = append(filtered, configProblems...)
	state.mu.Unlock()

	logNodeConfigProblemsIfDue(state, configProblems, logInterval, time.Now())
}

// getLocalSitePodCIDRs resolves local site pod CIDR pools from Site spec and falls back to the node pod CIDRs.
func getLocalSitePodCIDRs(siteInformer cache.SharedIndexInformer, siteName string, nodePodCIDRs []string) []string {
	collected := make([]string, 0, len(nodePodCIDRs))
	collected = append(collected, nodePodCIDRs...)

	trimmedSiteName := strings.TrimSpace(siteName)
	if siteInformer == nil || trimmedSiteName == "" {
		return dedupeStrings(collected)
	}

	for _, item := range siteInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site, err := parseSite(unstr)
		if err != nil {
			klog.Warningf("Failed to parse Site while collecting local pod CIDRs: %v", err)
			continue
		}

		if strings.TrimSpace(site.Name) != trimmedSiteName {
			continue
		}

		for _, assignment := range site.Spec.PodCidrAssignments {
			collected = append(collected, assignment.CidrBlocks...)
		}

		break
	}

	return dedupeStrings(collected)
}

// collectNodeConfigValidationErrors inspects host networking settings and returns node errors for unsafe values.
func collectNodeConfigValidationErrors(sitePodCIDRs []string, deps nodeConfigValidatorDeps) []NodeError {
	hasIPv4CIDR, hasIPv6CIDR := detectCIDRFamilies(sitePodCIDRs)
	problems := make([]NodeError, 0, 5)

	if hasIPv4CIDR && !readProcBoolEnabled("/proc/sys/net/ipv4/ip_forward", deps.readFile) {
		problems = append(problems, NodeError{
			Type:    nodeErrorTypeConfigIPv4ForwardingDisabled,
			Message: "ipv4 forwarding is disabled (net.ipv4.ip_forward=0)",
		})
	}

	if hasIPv6CIDR && !readProcBoolEnabled("/proc/sys/net/ipv6/conf/all/forwarding", deps.readFile) {
		problems = append(problems, NodeError{
			Type:    nodeErrorTypeConfigIPv6ForwardingDisabled,
			Message: "ipv6 forwarding is disabled (net.ipv6.conf.all.forwarding=0) while local site has IPv6 pod CIDRs",
		})
	}

	if forwardPolicyIsDrop("iptables", deps.runForwardPolicy) {
		problems = append(problems, NodeError{
			Type:    nodeErrorTypeConfigIptablesForwardDrop,
			Message: "iptables FORWARD default policy is DROP",
		})
	}

	if forwardPolicyIsDrop("ip6tables", deps.runForwardPolicy) {
		problems = append(problems, NodeError{
			Type:    nodeErrorTypeConfigIp6tablesForwardDrop,
			Message: "ip6tables FORWARD default policy is DROP",
		})
	}

	strictInterfaces := findStrictRPFilterInterfaces(deps)
	if len(strictInterfaces) > 0 {
		problems = append(problems, NodeError{
			Type: nodeErrorTypeConfigRPFilterStrict,
			Message: fmt.Sprintf(
				"rp_filter is set to strict mode (1) on: %s (expected 0 or 2)",
				strings.Join(strictInterfaces, ", "),
			),
		})
	}

	if err := checkConntrackUsage(deps.readFile); err != nil {
		problems = append(problems, NodeError{
			Type:    nodeErrorTypeConntrackNearMax,
			Message: err.Error(),
		})
	}

	return problems
}

// detectCIDRFamilies reports whether IPv4 and/or IPv6 CIDRs are present.
func detectCIDRFamilies(cidrs []string) (bool, bool) {
	hasIPv4 := false
	hasIPv6 := false

	for _, cidr := range cidrs {
		ip, _, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil || ip == nil {
			continue
		}

		if ip.To4() != nil {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}

		if hasIPv4 && hasIPv6 {
			return true, true
		}
	}

	return hasIPv4, hasIPv6
}

// readProcBoolEnabled checks whether a proc sysctl file is logically enabled (value "1").
func readProcBoolEnabled(path string, readFile func(string) ([]byte, error)) bool {
	if readFile == nil {
		return false
	}

	data, err := readFile(path)
	if err != nil {
		klog.V(4).Infof("Failed to read sysctl %q: %v", path, err)
		return false
	}

	return strings.TrimSpace(string(data)) == "1"
}

// forwardPolicyIsDrop checks whether the FORWARD chain default policy is DROP.
func forwardPolicyIsDrop(binary string, runForwardPolicy func(binary string) (string, error)) bool {
	if runForwardPolicy == nil {
		return false
	}

	out, err := runForwardPolicy(binary)
	if err != nil {
		klog.V(4).Infof("Failed to inspect %s FORWARD policy: %v", binary, err)
		return false
	}

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}

		if fields[0] == "-P" && fields[1] == "FORWARD" && strings.EqualFold(fields[2], "DROP") {
			return true
		}
	}

	return false
}

// findStrictRPFilterInterfaces returns interface names where rp_filter is set to 1.
func findStrictRPFilterInterfaces(deps nodeConfigValidatorDeps) []string {
	if deps.glob == nil || deps.readFile == nil {
		return nil
	}

	paths, err := deps.glob("/proc/sys/net/ipv4/conf/*/rp_filter")
	if err != nil {
		klog.V(4).Infof("Failed to list rp_filter sysctls: %v", err)
		return nil
	}

	interfaces := make([]string, 0, len(paths))
	ignoredInterfaces := map[string]struct{}{
		"all":     {},
		"default": {},
		"lo":      {},
	}

	for _, path := range paths {
		valueRaw, err := deps.readFile(path)
		if err != nil {
			klog.V(4).Infof("Failed to read rp_filter sysctl %q: %v", path, err)
			continue
		}

		if strings.TrimSpace(string(valueRaw)) != "1" {
			continue
		}

		interfaceName := filepath.Base(filepath.Dir(path))
		if interfaceName != "" {
			if _, ignored := ignoredInterfaces[interfaceName]; ignored {
				continue
			}

			interfaces = append(interfaces, interfaceName)
		}
	}

	slices.Sort(interfaces)

	return interfaces
}

// logNodeConfigProblemsIfDue prints detected config problems at the configured interval while issues persist.
func logNodeConfigProblemsIfDue(state *wireGuardState, problems []NodeError, logInterval time.Duration, now time.Time) {
	if state == nil {
		return
	}

	if len(problems) == 0 {
		state.mu.Lock()
		state.lastConfigProblemsLogTime = time.Time{}
		state.lastConfigProblemsLogSignature = ""
		state.mu.Unlock()

		return
	}

	if logInterval <= 0 {
		logInterval = time.Minute
	}

	signatureParts := make([]string, 0, len(problems))
	for _, problem := range problems {
		signatureParts = append(signatureParts, fmt.Sprintf("%s=%s", problem.Type, problem.Message))
	}

	signature := strings.Join(signatureParts, "|")

	state.mu.Lock()

	shouldLog := shouldLogNodeConfigProblems(
		state.lastConfigProblemsLogTime,
		state.lastConfigProblemsLogSignature,
		signature,
		logInterval,
		now,
	)
	if shouldLog {
		state.lastConfigProblemsLogTime = now
		state.lastConfigProblemsLogSignature = signature
	}
	state.mu.Unlock()

	if !shouldLog {
		return
	}

	klog.Warningf("Node configuration validation reported %d problem(s):", len(problems))

	for _, problem := range problems {
		klog.Warningf("  - [%s] %s", problem.Type, problem.Message)
	}
}

// shouldLogNodeConfigProblems reports whether config problems should be logged now.
func shouldLogNodeConfigProblems(lastLoggedAt time.Time, lastSignature, signature string, interval time.Duration, now time.Time) bool {
	if strings.TrimSpace(signature) == "" {
		return false
	}

	if lastLoggedAt.IsZero() {
		return true
	}

	if signature != lastSignature {
		return true
	}

	return now.Sub(lastLoggedAt) >= interval
}

// checkConntrackUsage reads nf_conntrack_count and nf_conntrack_max and
// returns an error if usage is within 20% of the maximum.
func checkConntrackUsage(readFile func(string) ([]byte, error)) error {
	if readFile == nil {
		return nil
	}

	count, countErr := readProcInt("/proc/sys/net/netfilter/nf_conntrack_count", readFile)

	max, maxErr := readProcInt("/proc/sys/net/netfilter/nf_conntrack_max", readFile)
	if countErr != nil || maxErr != nil || max <= 0 {
		return nil
	}

	threshold := int(float64(max) * conntrackUsageThreshold)
	if count >= threshold {
		pct := float64(count) / float64(max) * 100
		return fmt.Errorf("conntrack table usage is %.0f%% (%d/%d) -- approaching nf_conntrack_max", pct, count, max)
	}

	return nil
}

// readProcInt reads a proc file and parses it as an integer.
func readProcInt(path string, readFile func(string) ([]byte, error)) (int, error) {
	data, err := readFile(path)
	if err != nil {
		return 0, err
	}

	var val int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &val); err != nil {
		return 0, err
	}

	return val, nil
}
