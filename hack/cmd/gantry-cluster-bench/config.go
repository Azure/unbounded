// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// runGantryConfig reads a gantry config.yaml on stdin, makes sure the bench
// origin is listed in upstream_registries, applies --set overrides, and
// writes the result to stdout. It backs deploy.sh, which edits the
// operator-owned ConfigMap/gantry-config.
func runGantryConfig(args []string) error {
	fs := flag.NewFlagSet("gantry-config", flag.ContinueOnError)
	origin := fs.String("origin", "", "origin registry host:port (required)")
	scheme := fs.String("scheme", "http", "origin endpoint scheme")

	var sets multiFlag

	fs.Var(&sets, "set", "top-level key=value override (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	out, err := patchGantryConfig(in, *origin, *scheme, sets)
	if err != nil {
		return err
	}

	_, err = os.Stdout.Write(out)

	return err
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// patchGantryConfig returns cfg with an upstream_registries entry named
// origin (endpoint scheme://origin) and each key=value of sets applied as a
// top-level YAML value. An existing entry with the same name is replaced, so
// the patch is idempotent. Comments in cfg are not preserved.
func patchGantryConfig(cfg []byte, origin, scheme string, sets []string) ([]byte, error) {
	if origin == "" || strings.ContainsAny(origin, "/ ") {
		return nil, fmt.Errorf("origin must be a bare host:port, got %q", origin)
	}

	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https, got %q", scheme)
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		return nil, fmt.Errorf("parse gantry config: %w", err)
	}

	if doc == nil {
		doc = map[string]any{}
	}

	var regs []any

	switch v := doc["upstream_registries"].(type) {
	case nil:
	case []any:
		regs = v
	default:
		return nil, errors.New("upstream_registries is not a list")
	}

	kept := make([]any, 0, len(regs)+1)

	for _, r := range regs {
		if m, ok := r.(map[string]any); ok && m["name"] == origin {
			continue
		}

		kept = append(kept, r)
	}

	kept = append(kept, map[string]any{"name": origin, "endpoint": scheme + "://" + origin})
	doc["upstream_registries"] = kept

	for _, s := range sets {
		key, raw, ok := strings.Cut(s, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--set %q: want key=value", s)
		}

		var val any
		if err := yaml.Unmarshal([]byte(raw), &val); err != nil {
			return nil, fmt.Errorf("--set %q: %w", s, err)
		}

		doc[key] = val
	}

	return yaml.Marshal(doc)
}
