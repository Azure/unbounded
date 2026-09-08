// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

// deprecatedSiteLabelKey is the node site-membership label used by released net
// controllers before the switch to unbounded-cloud.io/site. It is re-exported
// for the legacy reaper (see migrate.go).
const deprecatedSiteLabelKey = component.DeprecatedSiteLabelKey

// configMapPayloadHash forwards to component.ConfigMapPayloadHash for the legacy
// reaper (see migrate.go).
func configMapPayloadHash(config *corev1.ConfigMap) string {
	return component.ConfigMapPayloadHash(config)
}
