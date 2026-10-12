// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestContainerBuilderImage(t *testing.T) {
	builder := regexp.MustCompile(`(?m)^FROM --platform=\$BUILDPLATFORM (docker\.io/library/golang:\S+) AS builder$`)
	images := make(map[string]string)

	for _, name := range []string{"gantry", "racer-object"} {
		path := filepath.Join("..", "..", "images", name, "Containerfile")

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		match := builder.FindSubmatch(data)
		if len(match) != 2 {
			t.Fatalf("%s: missing Go builder image", path)
		}

		images[name] = string(match[1])
	}

	if images["racer-object"] != images["gantry"] {
		t.Fatalf("racer-object builder = %q, want %q to match gantry", images["racer-object"], images["gantry"])
	}
}
