// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"strings"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	maxConditionReasonBytes  = 1024
	maxConditionMessageBytes = 32768
	defaultConditionReason   = "ProviderStatus"
)

func normalizeCondition(condition metav1.Condition) metav1.Condition {
	condition.Reason = normalizeConditionReason(condition.Reason)
	condition.Message = truncateUTF8Bytes(condition.Message, maxConditionMessageBytes)

	return condition
}

func normalizeConditionReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return defaultConditionReason
	}

	var normalized strings.Builder
	normalized.Grow(min(len(reason), maxConditionReasonBytes))

	for i := 0; i < len(reason) && normalized.Len() < maxConditionReasonBytes; i++ {
		character := reason[i]
		if isConditionReasonCharacter(character) {
			normalized.WriteByte(character)
			continue
		}

		if normalized.Len() > 0 {
			normalized.WriteByte('_')
		}
	}

	value := strings.TrimRight(normalized.String(), ",:")
	if value == "" {
		return defaultConditionReason
	}

	if !isASCIILetter(value[0]) {
		value = "Provider_" + value
	}

	if len(value) > maxConditionReasonBytes {
		value = strings.TrimRight(value[:maxConditionReasonBytes], ",:")
	}

	return value
}

func isConditionReasonCharacter(character byte) bool {
	return isASCIILetter(character) ||
		(character >= '0' && character <= '9') ||
		character == '_' || character == ',' || character == ':'
}

func isASCIILetter(character byte) bool {
	return (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z')
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}

	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}

	return value[:end]
}
