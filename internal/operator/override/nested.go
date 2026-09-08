// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The helpers below wrap the unstructured accessors for reads where a missing
// field and a field of the wrong shape are both simply "not there".
//
// A patch is user input, so a type mismatch is a routine possibility rather
// than a programming error. Treating it as absent here keeps the read sites
// readable, and anything genuinely wrong is caught by validation or by the
// API server on apply.

func nestedSlice(obj map[string]any, fields ...string) []any {
	value, found, err := unstructured.NestedSlice(obj, fields...)
	if err != nil || !found {
		return nil
	}

	return value
}

func nestedMap(obj map[string]any, fields ...string) map[string]any {
	value, found, err := unstructured.NestedMap(obj, fields...)
	if err != nil || !found {
		return nil
	}

	return value
}

func nestedStringMap(obj map[string]any, fields ...string) map[string]string {
	value, found, err := unstructured.NestedStringMap(obj, fields...)
	if err != nil || !found {
		return nil
	}

	return value
}

// setNestedSlice writes a nested slice, reporting the error so callers decide.
func setNestedSlice(obj map[string]any, value []any, fields ...string) error {
	return unstructured.SetNestedSlice(obj, value, fields...)
}

// setNestedMap writes a nested map, reporting the error so callers decide.
func setNestedMap(obj, value map[string]any, fields ...string) error {
	return unstructured.SetNestedMap(obj, value, fields...)
}
