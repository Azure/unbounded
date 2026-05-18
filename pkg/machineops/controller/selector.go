// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// SelectorMatches returns true when selector matches targetLabels.
func SelectorMatches(selector *metav1.LabelSelector, targetLabels map[string]string) (bool, error) {
	compiled, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}

	return compiled.Matches(labels.Set(targetLabels)), nil
}
