// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"
	"time"
)

// formatAge formats elapsed time since t as a human-readable duration
// (e.g. "5m", "2h", "3d").
func formatAge(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
