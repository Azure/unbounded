// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	maxK8sNameLength  = 253
	maxK8sLabelLength = 63
)

// nonK8sNameChars matches any character that is not a lowercase alphanumeric,
// dash, or dot - i.e. characters not allowed in a Kubernetes object name.
var nonK8sNameChars = regexp.MustCompile(`[^a-z0-9\-.]`)

// SanitizeK8sName converts a raw string into a valid Kubernetes object name.
// Each dot-separated label is lowercased, characters outside [a-z0-9-] are
// replaced with '-', runs of '-' are collapsed, and leading/trailing '-'
// characters are trimmed. Empty labels are dropped, each label is limited to
// 63 bytes, and the whole name is limited to 253 bytes. If the input has no
// usable characters, the result is a stable name derived from a hash of the
// input. The returned string is always a non-empty RFC-1123 subdomain.
func SanitizeK8sName(raw string) string {
	s := strings.ToLower(raw)

	// Replace any character that is not alphanumeric, '-', or '.' with '-'.
	s = nonK8sNameChars.ReplaceAllString(s, "-")

	labels := strings.Split(s, ".")
	sanitized := make([]string, 0, len(labels))

	for _, label := range labels {
		label = collapseDashes(label)
		label = strings.Trim(label, "-")

		if len(label) > maxK8sLabelLength {
			label = strings.TrimRight(label[:maxK8sLabelLength], "-")
		}

		if label != "" {
			sanitized = append(sanitized, label)
		}
	}

	s = strings.Join(sanitized, ".")
	if s == "" {
		return fallbackK8sName(raw)
	}

	// Truncate to 253 characters. The first byte is alphanumeric, so trimming
	// the trailing separators cannot make the result empty.
	if len(s) > maxK8sNameLength {
		s = strings.TrimRight(s[:maxK8sNameLength], "-.")
	}

	return s
}

// collapseDashes replaces runs of consecutive dashes with a single dash.
func collapseDashes(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}

	return s
}

// fallbackK8sName derives a stable, valid name from raw when sanitization
// removes every usable character.
func fallbackK8sName(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return "name-" + hex.EncodeToString(sum[:4])
}
