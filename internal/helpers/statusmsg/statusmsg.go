// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package statusmsg keeps messages written to Kubernetes status within what the
// API server accepts. A status write with an oversized message is rejected
// whole, so every component that writes conditions bounds its messages first.
package statusmsg

import "unicode/utf8"

// conditionMaxBytes is the longest metav1.Condition message the API server
// accepts. It mirrors maxMessageLen in
// k8s.io/apimachinery/pkg/apis/meta/v1/validation, which is not exported.
const conditionMaxBytes = 32 * 1024

// TruncateCondition returns msg cut to the longest metav1.Condition message the
// API server accepts, without splitting a UTF-8 sequence.
func TruncateCondition(msg string) string {
	return Truncate(msg, conditionMaxBytes)
}

// Truncate returns msg cut to at most maxBytes bytes without splitting a UTF-8
// sequence. It panics if maxBytes is negative.
func Truncate(msg string, maxBytes int) string {
	if maxBytes < 0 {
		panic("statusmsg: negative Truncate maxBytes")
	}

	if len(msg) <= maxBytes {
		return msg
	}

	end := maxBytes
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}

	return msg[:end]
}
