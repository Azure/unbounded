// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"fmt"
	"strconv"
	"strings"
)

// parseSize converts a human-readable size string into a byte count.
// Supports the following suffixes (case-insensitive): B, KB, KiB, MB,
// MiB, GB, GiB, TB, TiB. Decimal suffixes (KB, MB, ...) use base 1000;
// binary suffixes (KiB, MiB, ...) use base 1024. Bare numbers are
// interpreted as bytes.
//
// Examples:
//
//	"1024"   -> 1024
//	"1KB"    -> 1000
//	"1KiB"   -> 1024
//	"10MiB"  -> 10485760
//	"1.5GB"  -> 1500000000
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}
	// Walk forward to find the numeric / suffix split.
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' {
			i++
			continue
		}

		break
	}

	if i == 0 {
		return 0, fmt.Errorf("size %q has no numeric prefix", s)
	}

	numStr := s[:i]
	suffix := strings.ToLower(strings.TrimSpace(s[i:]))

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q: %w", numStr, err)
	}

	if num < 0 {
		return 0, fmt.Errorf("size must be non-negative, got %s", numStr)
	}

	var mult int64

	switch suffix {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1000
	case "ki", "kib":
		mult = 1024
	case "m", "mb":
		mult = 1000 * 1000
	case "mi", "mib":
		mult = 1024 * 1024
	case "g", "gb":
		mult = 1000 * 1000 * 1000
	case "gi", "gib":
		mult = 1024 * 1024 * 1024
	case "t", "tb":
		mult = 1000 * 1000 * 1000 * 1000
	case "ti", "tib":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("size %q has unrecognized suffix %q (want B, KB/KiB, MB/MiB, GB/GiB, TB/TiB)", s, suffix)
	}

	return int64(num * float64(mult)), nil
}

// formatSize renders a byte count as a human-friendly string using
// binary suffixes (KiB, MiB, GiB). Used in progress and summary
// output where readability matters more than precision.
func formatSize(n int64) string {
	const (
		kib int64 = 1024
		mib int64 = 1024 * kib
		gib int64 = 1024 * mib
		tib int64 = 1024 * gib
	)

	switch {
	case n >= tib:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
