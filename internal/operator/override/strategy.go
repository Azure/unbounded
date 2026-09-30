// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func validateDaemonSetStrategy(entry Entry) []string {
	if entry.Kind != "DaemonSet" {
		return nil
	}

	strategyType, found, err := unstructured.NestedString(entry.Patch, "spec", "updateStrategy", "type")
	if err != nil {
		return []string{"spec.updateStrategy.type must be a string"}
	}

	if !found {
		return nil
	}

	switch appsv1.DaemonSetUpdateStrategyType(strategyType) {
	case appsv1.RollingUpdateDaemonSetStrategyType:
		return nil
	case appsv1.OnDeleteDaemonSetStrategyType:
		if _, present, err := unstructured.NestedFieldNoCopy(entry.Patch, "spec", "updateStrategy", "rollingUpdate"); err != nil {
			return []string{fmt.Sprintf("read spec.updateStrategy.rollingUpdate: %v", err)}
		} else if present {
			return []string{"spec.updateStrategy cannot set OnDelete and rollingUpdate together; omit rollingUpdate for OnDelete"}
		}
	default:
		return []string{"spec.updateStrategy.type must be RollingUpdate or OnDelete"}
	}

	return nil
}

// normalizeDaemonSetStrategy runs after all validated contributors have merged,
// before the candidate enters the apply plan. Only the operator's inherited
// rolling block may be removed; user-supplied rolling settings must not silently
// disappear, including when a different contributor selected OnDelete.
func normalizeDaemonSetStrategy(workload *unstructured.Unstructured, contributors []SourcedEntry) error {
	if workload.GetKind() != "DaemonSet" {
		return nil
	}

	strategyType, _, err := unstructured.NestedString(workload.Object, "spec", "updateStrategy", "type")
	if err != nil {
		return fmt.Errorf("read spec.updateStrategy.type: %w", err)
	}

	if strategyType != string(appsv1.OnDeleteDaemonSetStrategyType) {
		return nil
	}

	for _, contributor := range contributors {
		if _, present, err := unstructured.NestedFieldNoCopy(contributor.Entry.Patch, "spec", "updateStrategy", "rollingUpdate"); err != nil {
			return fmt.Errorf("%s: read spec.updateStrategy.rollingUpdate: %w", contributor.Source, err)
		} else if present {
			return fmt.Errorf("%s: spec.updateStrategy.rollingUpdate conflicts with the merged OnDelete strategy; omit rollingUpdate for OnDelete", contributor.Source)
		}
	}

	unstructured.RemoveNestedField(workload.Object, "spec", "updateStrategy", "rollingUpdate")

	return nil
}
