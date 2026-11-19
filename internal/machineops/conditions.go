// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/helpers/statusmsg"
)

const (
	maxConditionReasonBytes = 1024
	defaultConditionReason  = "ProviderStatus"
)

func normalizeCondition(condition metav1.Condition) metav1.Condition {
	condition.Reason = normalizeConditionReason(condition.Reason)
	condition.Message = statusmsg.Truncate(condition.Message, statusmsg.MaxBytes)

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
