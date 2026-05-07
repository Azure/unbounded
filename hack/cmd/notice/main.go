// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// notice generates and verifies the project's NOTICE file from the direct
// dependencies declared in go.mod and frontend/package.json.
//
// Subcommands:
//
//	notice generate   Render NOTICE from current dependencies.
//	notice check      Verify on-disk NOTICE matches what would be rendered.
//
// To add a new ecosystem (e.g. PyPI, Cargo) implement the
// notice.Collector interface in a new internal/<name>/ package and append
// <name>.New() to the collectors slice below. See README.md for details.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/gomod"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/npm"
)

// collectors is the explicit list of ecosystems the tool knows about. To
// add a new ecosystem, implement notice.Collector and append it here.
func collectors() []notice.Collector {
	return []notice.Collector{
		gomod.New(),
		npm.New(),
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "generate":
		if err := runGenerate(args); err != nil {
			fmt.Fprintf(os.Stderr, "notice generate: %v\n", err)
			os.Exit(1)
		}
	case "check":
		if err := runCheck(args); err != nil {
			fmt.Fprintf(os.Stderr, "notice check: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: notice <subcommand> [flags]

Subcommands:
  generate   Render NOTICE from go.mod and frontend/package.json.
  check      Verify on-disk NOTICE matches what would be rendered.

Common flags (defaults shown):
  --repo-root .              Project root containing go.mod and frontend/.
  --output    NOTICE         Output file path (generate only).
  --notice    NOTICE         File to compare against (check only).
`)
}

func runGenerate(args []string) error {
	root, output, err := parseGenerateFlags(args)
	if err != nil {
		return err
	}

	doc, err := notice.Build(root, collectors())
	if err != nil {
		return err
	}

	out, err := notice.Render(doc)
	if err != nil {
		return err
	}

	if err := os.WriteFile(output, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", output, err)
	}

	fmt.Fprintf(os.Stderr, "wrote %s (%d entries)\n", output, len(doc.Notices))

	return nil
}

func runCheck(args []string) error {
	root, target, err := parseCheckFlags(args)
	if err != nil {
		return err
	}

	doc, err := notice.Build(root, collectors())
	if err != nil {
		return err
	}

	want, err := notice.Render(doc)
	if err != nil {
		return err
	}

	got, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("reading %s: %w", target, err)
	}

	if bytes.Equal(want, got) {
		fmt.Fprintf(os.Stderr, "%s is up to date (%d entries)\n", target, len(doc.Notices))
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s is out of date. Run 'make notice' to regenerate.\n\n", target)
	fmt.Fprint(os.Stderr, notice.Diff(string(got), string(want)))

	return errors.New("NOTICE drift detected")
}

func parseGenerateFlags(args []string) (root, output string, err error) {
	root = "."
	output = "NOTICE"

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--repo-root":
			i++
			if i >= len(args) {
				return "", "", fmt.Errorf("--repo-root requires a value")
			}

			root = args[i]
		case strings.HasPrefix(a, "--repo-root="):
			root = strings.TrimPrefix(a, "--repo-root=")
		case a == "--output":
			i++
			if i >= len(args) {
				return "", "", fmt.Errorf("--output requires a value")
			}

			output = args[i]
		case strings.HasPrefix(a, "--output="):
			output = strings.TrimPrefix(a, "--output=")
		default:
			return "", "", fmt.Errorf("unknown flag: %s", a)
		}
	}

	return root, output, nil
}

func parseCheckFlags(args []string) (root, target string, err error) {
	root = "."
	target = "NOTICE"

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--repo-root":
			i++
			if i >= len(args) {
				return "", "", fmt.Errorf("--repo-root requires a value")
			}

			root = args[i]
		case strings.HasPrefix(a, "--repo-root="):
			root = strings.TrimPrefix(a, "--repo-root=")
		case a == "--notice":
			i++
			if i >= len(args) {
				return "", "", fmt.Errorf("--notice requires a value")
			}

			target = args[i]
		case strings.HasPrefix(a, "--notice="):
			target = strings.TrimPrefix(a, "--notice=")
		default:
			return "", "", fmt.Errorf("unknown flag: %s", a)
		}
	}

	return root, target, nil
}
