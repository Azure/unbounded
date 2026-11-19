// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package statusmsg keeps messages written to Kubernetes status within what the
// API server accepts. A status write with an oversized message is rejected
// whole, so every component that writes conditions bounds its messages first.
package statusmsg

import "unicode/utf8"

// MaxBytes is the longest metav1.Condition message the API server accepts.
const MaxBytes = 32768

// Truncate returns msg cut to at most maxBytes bytes without splitting a UTF-8
// sequence.
func Truncate(msg string, maxBytes int) string {
	if len(msg) <= maxBytes {
		return msg
	}

	end := maxBytes
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}

	return msg[:end]
}
