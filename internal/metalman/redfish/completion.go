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

// completion checks vendor Oem objects in key order. Any code other than
// Success is an error. It reports whether the message had at least one code.
func (m completionMessage) completion() (bool, error) {
	keys := make([]string, 0, len(m.Oem))
	for key := range m.Oem {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	found := false

	for _, key := range keys {
		var fields map[string]json.RawMessage
		if json.Unmarshal(m.Oem[key], &fields) != nil {
			continue
		}

		raw, ok := fields["CompletionCode"]
		if !ok {
			continue
		}

		var code *string
		if err := json.Unmarshal(raw, &code); err != nil {
			return false, fmt.Errorf("parsing %s completion code: %w", key, err)
		}

		if code == nil {
			return false, fmt.Errorf("parsing %s completion code: expected a string", key)
		}

		if *code == "Failure" {
			// The description is optional context; ignore missing or non-string values.
			var description string
			if json.Unmarshal(fields["Description"], &description) != nil {
				description = ""
			}

			return false, &completionFailure{description: description}
		}

		if *code != "Success" {
			return false, fmt.Errorf("unexpected %s completion code %q", key, *code)
		}

		found = true
	}

	return found, nil
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
			if _, err := message.completion(); err != nil {
				return err
			}
		}
	}

	return nil
}
