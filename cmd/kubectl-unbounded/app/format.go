// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"time"

	"k8s.io/apimachinery/pkg/util/duration"
)

func formatAge(t time.Time) string {
	return duration.HumanDuration(time.Since(t))
}
