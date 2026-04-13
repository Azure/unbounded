// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"regexp"
	"strings"
)

// nonK8sNameChars matches any character that is not a lowercase alphanumeric,
// dash, or dot - i.e. characters not allowed in a Kubernetes object name.
var nonK8sNameChars = regexp.MustCompile(`[^a-z0-9\-.]`)

// SanitizeK8sName converts a raw string into a valid Kubernetes object name.
// Kubernetes names must be lowercase RFC-1123 subdomains: lowercase alphanumeric
// characters, '-' or '.', must start and end with an alphanumeric character,
// and be at most 253 characters.
func SanitizeK8sName(raw string) string {
	s := strings.ToLower(raw)

	// Replace any character that is not alphanumeric, '-', or '.' with '-'.
	s = nonK8sNameChars.ReplaceAllString(s, "-")

	// Collapse consecutive dashes.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}

	// Trim leading/trailing non-alphanumeric characters.
	s = strings.TrimLeft(s, "-.")
	s = strings.TrimRight(s, "-.")

	// Truncate to 253 characters.
	if len(s) > 253 {
		s = s[:253]
		s = strings.TrimRight(s, "-.")
	}

	return s
}
