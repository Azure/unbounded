// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const excludedExample = "gpu-identity-example.sh"

type scriptContract struct {
	Commit  string            `json:"commit"`
	Scripts map[string]string `json:"scripts"`
}

func main() {
	var (
		namespace    string
		scriptsDir   string
		contractPath string
		outputPath   string
	)

	flag.StringVar(&namespace, "namespace", "project-signal", "ConfigMap namespace")
	flag.StringVar(&scriptsDir, "scripts-dir", "project-signal-certification/scripts", "certification script directory")
	flag.StringVar(&contractPath, "contract", "project-signal-certification-runner/script-contract.json", "script contract")
	flag.StringVar(&outputPath, "output", "deploy/signal/rendered/03-certification-scripts.yaml", "rendered ConfigMap path")
	flag.Parse()

	if err := render(namespace, scriptsDir, contractPath, outputPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func render(namespace, scriptsDir, contractPath, outputPath string) error {
	raw, err := os.ReadFile(contractPath)
	if err != nil {
		return fmt.Errorf("read script contract: %w", err)
	}

	var contract scriptContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		return fmt.Errorf("decode script contract: %w", err)
	}

	names := make([]string, 0, len(contract.Scripts)-1)
	contents := make(map[string]string, len(contract.Scripts)-1)

	for name := range contract.Scripts {
		if name == excludedExample {
			continue
		}

		path := filepath.Join(scriptsDir, name)

		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}

		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			return fmt.Errorf("%s must be a regular 0755 file", name)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}

		digest := fmt.Sprintf("%x", sha256.Sum256(content))
		if digest != contract.Scripts[name] {
			return fmt.Errorf("%s sha256 = %s, want %s", name, digest, contract.Scripts[name])
		}

		names = append(names, name)
		contents[name] = string(content)
	}

	sort.Strings(names)

	var output strings.Builder
	fmt.Fprintf(
		&output,
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: project-signal-certification-scripts\n  namespace: %s\n  annotations:\n    signal.unbounded-cloud.io/script-commit: %q\nimmutable: true\ndata:\n",
		namespace,
		contract.Commit,
	)

	for _, name := range names {
		fmt.Fprintf(&output, "  %s: |\n", name)

		for _, line := range strings.SplitAfter(contents[name], "\n") {
			if line == "" {
				continue
			}

			output.WriteString("    ")
			output.WriteString(line)

			if !strings.HasSuffix(line, "\n") {
				output.WriteByte('\n')
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(output.String()), 0o644); err != nil {
		return fmt.Errorf("write ConfigMap: %w", err)
	}

	return nil
}
