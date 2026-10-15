// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"regexp"
	"strings"
)

const maxDisplayValue = 2048

var (
	gpuUUIDPattern = regexp.MustCompile(`(?i)\bGPU-[0-9a-f-]{8,}\b`)
	uuidPattern    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`)
	longHexPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{48,}\b`)
	urlPattern     = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
)

func sanitizeDisplay(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = gpuUUIDPattern.ReplaceAllString(value, "GPU-[redacted]")
	value = uuidPattern.ReplaceAllString(value, "[id-redacted]")
	value = longHexPattern.ReplaceAllString(value, "[digest-redacted]")
	value = urlPattern.ReplaceAllString(value, "[endpoint-redacted]")

	if len(value) > maxDisplayValue {
		value = value[:maxDisplayValue] + "...[truncated]"
	}

	return value
}

func shortDigest(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 15 {
		return value
	}

	return value[:15] + "..."
}
