// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"encoding/json"
	"fmt"
)

type completionMessage struct {
	Oem struct {
		Microsoft struct {
			CompletionCode string
			Description    string
		} `json:"Microsoft"`
	} `json:"Oem"`
}

type completionFailure struct {
	description string
}

func (e *completionFailure) Error() string {
	return "Microsoft CompletionCode Failure: " + e.description
}

// checkCompletion only checks the Microsoft status extension, not the error key.
// Some rack managers put successful command results inside error.
func checkCompletion(data []byte) error {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return nil
	}

	containers := []map[string]json.RawMessage{root}

	var nested map[string]json.RawMessage
	if json.Unmarshal(root["error"], &nested) == nil {
		containers = append(containers, nested)
	}

	for _, container := range containers {
		raw, ok := container["@Message.ExtendedInfo"]
		if !ok {
			continue
		}

		var messages []completionMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return fmt.Errorf("parsing Microsoft completion status: %w", err)
		}

		for _, message := range messages {
			if message.Oem.Microsoft.CompletionCode == "Failure" {
				return &completionFailure{description: message.Oem.Microsoft.Description}
			}
		}
	}

	return nil
}
