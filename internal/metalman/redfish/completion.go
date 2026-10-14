// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"encoding/json"
	"fmt"
	"slices"
)

// completionMessage is one @Message.ExtendedInfo entry. Some rack managers
// report the command result as a CompletionCode inside a vendor Oem object.
// The vendor key is not fixed, so every Oem object is checked.
type completionMessage struct {
	Oem map[string]json.RawMessage `json:"Oem"`
}

// completion returns the first CompletionCode found in the vendor Oem
// objects, in key order. It returns ok false if there is none.
func (m completionMessage) completion() (code, description string, ok bool, err error) {
	keys := make([]string, 0, len(m.Oem))
	for key := range m.Oem {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		var fields map[string]json.RawMessage
		if json.Unmarshal(m.Oem[key], &fields) != nil {
			continue
		}

		raw, found := fields["CompletionCode"]
		if !found {
			continue
		}

		if err := json.Unmarshal(raw, &code); err != nil {
			return "", "", false, fmt.Errorf("parsing %s completion code: %w", key, err)
		}

		// The description is optional context. A missing or non-string value
		// leaves it empty.
		if raw, found := fields["Description"]; found {
			if json.Unmarshal(raw, &description) != nil {
				description = ""
			}
		}

		return code, description, true, nil
	}

	return "", "", false, nil
}

type completionFailure struct {
	description string
}

func (e *completionFailure) Error() string {
	return "completion code Failure: " + e.description
}

// checkCompletion only checks vendor completion codes, not the error key.
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
			return fmt.Errorf("parsing completion status: %w", err)
		}

		for _, message := range messages {
			code, description, ok, err := message.completion()
			if err != nil {
				return err
			}

			if ok && code == "Failure" {
				return &completionFailure{description: description}
			}
		}
	}

	return nil
}
