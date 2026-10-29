// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// The image's cufile.json must parse and agree with the workload wiring.
func TestImageCUFileConfig(t *testing.T) {
	raw, err := os.ReadFile("../../../images/racer-dataplane/cufile.json")
	if err != nil {
		t.Fatal(err)
	}

	// cuFile accepts // line comments; no value in the file contains "//".
	stripped := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAll(raw, nil)

	var config struct {
		Logging struct {
			Dir   string `json:"dir"`
			Level string `json:"level"`
		} `json:"logging"`
		Properties struct {
			AllowCompatMode *bool `json:"allow_compat_mode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(stripped, &config); err != nil {
		t.Fatalf("cufile.json: %v", err)
	}

	if config.Logging.Dir != CUFileLogDirectory {
		t.Fatalf("logging.dir = %q, want %q", config.Logging.Dir, CUFileLogDirectory)
	}

	if config.Logging.Level != "ERROR" {
		t.Fatalf("logging.level = %q, want ERROR", config.Logging.Level)
	}

	if config.Properties.AllowCompatMode == nil || *config.Properties.AllowCompatMode {
		t.Fatal("properties.allow_compat_mode must be false")
	}
}
