// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"fmt"
	"net"
	"strings"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// routeKind classifies route type in the same spirit as the UI.
func routeKind(destination string, hop statusv1alpha1.NextHop) string {
	dest := strings.TrimSpace(destination)
	dev := strings.ToLower(strings.TrimSpace(hop.Device))

	if dest == "169.254.169.254/32" {
		return "IMDS"
	}

	if strings.HasPrefix(dev, "lo") || strings.HasPrefix(dev, "eth") || strings.HasPrefix(dev, "ens") || strings.HasPrefix(dev, "enp") {
		return "Local"
	}

	if isTunnelDevice(dev) {
		if strings.HasSuffix(dest, "/32") || strings.HasSuffix(dest, "/128") {
			return "Tunnel peer"
		}

		return "Tunnel gateway"
	}

	return "Other"
}

// isTunnelDevice returns true for any managed tunnel interface:
// WireGuard (wg*), GENEVE (gn*), VXLAN (vxlan*), and IPIP (ipip*).
func isTunnelDevice(dev string) bool {
	return strings.HasPrefix(dev, "wg") ||
		strings.HasPrefix(dev, "gn") ||
		strings.HasPrefix(dev, "vxlan") ||
		strings.HasPrefix(dev, "ipip")
}

// isTunnelKind reports whether the route kind is a managed tunnel route.
func isTunnelKind(kind string) bool {
	return kind == "Tunnel peer" || kind == "Tunnel gateway"
}

// isWireguardKind reports whether the route kind is a tunnel route.
// Kept for backward compatibility; delegates to isTunnelKind.
func isWireguardKind(kind string) bool {
	return isTunnelKind(kind)
}

// joinOrDash joins values or returns "-".
func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}

	out := []string{}

	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}

	if len(out) == 0 {
		return "-"
	}

	return strings.Join(out, ", ")
}

// valueOr returns fallback when value is empty.
func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}

	return v
}

// formatAgePtr formats age for time pointers.
func formatAgePtr(t *time.Time) string {
	if t == nil {
		return "Never"
	}

	return formatAge(*t)
}

// firstUsableIPs computes first usable host IP values for CIDRs.
func firstUsableIPs(cidrs []string) []string {
	out := []string{}

	for _, cidr := range cidrs {
		if ip := firstUsableIPFromCIDR(cidr); ip != "" {
			out = append(out, ip)
		}
	}

	return out
}

// firstUsableIPFromCIDR computes the first usable host IP for a CIDR.
func firstUsableIPFromCIDR(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}

	if ip.To4() != nil {
		ip4 := ip.Mask(ipNet.Mask).To4()
		if ip4 == nil {
			return ""
		}

		ip4[3]++

		return ip4.String()
	}

	ip16 := ip.Mask(ipNet.Mask).To16()
	if ip16 == nil {
		return ""
	}

	ip16[15]++

	return ip16.String()
}

// joinColumns prints fixed-width columns with two spaces between each column.
func joinColumns(values []string, widths []int) string {
	var b strings.Builder

	for i := range values {
		if i > 0 {
			b.WriteString("  ")
		}

		b.WriteString(values[i])

		pad := widths[i] - visibleLen(values[i])
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}

	return b.String()
}

// visibleLen returns display length excluding ANSI escape sequences.
func visibleLen(s string) int {
	n := 0
	inEsc := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inEsc {
			if ch == 'm' {
				inEsc = false
			}

			continue
		}

		if ch == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i++

			continue
		}

		n++
	}

	return n
}

// maxInt returns the larger integer.
func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}

// colorize wraps text with ANSI color codes for green/yellow/red.
func colorize(text, tone string) string {
	const reset = "\033[0m"

	switch tone {
	case "green":
		return "\033[32m" + text + reset
	case "yellow":
		return "\033[33m" + text + reset
	case "red":
		return "\033[31m" + text + reset
	case "dim":
		return "\033[2m" + text + reset
	default:
		return text
	}
}

// formatAge formats elapsed time as seconds/minutes/hours/days ago.
func formatAge(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "Never"
	}

	diff := time.Since(t)
	if diff < time.Minute {
		return fmt.Sprintf("%ds ago", int(diff.Seconds()))
	}

	if diff < time.Hour {
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	}

	if diff < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	}

	return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
}

// ptrInt64 returns a pointer to the provided int64.
func ptrInt64(v int64) *int64 {
	return &v
}
