// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type options struct {
	source          string
	output          string
	imageRepository string
}

func main() {
	var opts options

	flag.StringVar(&opts.source, "source", "", "Source Helm chart directory")
	flag.StringVar(&opts.output, "output", "", "Staged Helm chart directory")
	flag.StringVar(&opts.imageRepository, "image-repository", "", "Default Gantry image repository")
	flag.Parse()

	if err := stageChart(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func stageChart(opts options) error {
	if err := opts.validate(); err != nil {
		return err
	}

	if _, err := os.Stat(opts.output); err == nil {
		return fmt.Errorf("output directory %q already exists", opts.output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat output directory: %w", err)
	}

	if err := copyTree(opts.source, opts.output); err != nil {
		return err
	}

	valuesPath := filepath.Join(opts.output, "values.yaml")

	raw, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("read staged values: %w", err)
	}

	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return fmt.Errorf("decode staged values: %w", err)
	}

	image, ok := values["image"].(map[string]any)
	if !ok {
		return errors.New("staged values image must be an object")
	}

	image["repository"] = opts.imageRepository

	raw, err = yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode staged values: %w", err)
	}

	if err := os.WriteFile(valuesPath, raw, 0o644); err != nil {
		return fmt.Errorf("write staged values: %w", err)
	}

	return nil
}

func (o options) validate() error {
	switch {
	case strings.TrimSpace(o.source) == "":
		return errors.New("--source is required")
	case strings.TrimSpace(o.output) == "":
		return errors.New("--output is required")
	case strings.TrimSpace(o.imageRepository) == "":
		return errors.New("--image-repository is required")
	default:
		return nil
	}
}

func copyTree(source, output string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(source, path)
		if err != nil {
			return fmt.Errorf("resolve chart path %q: %w", path, err)
		}

		target := filepath.Join(output, relative)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create chart directory %q: %w", target, err)
			}

			return nil
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("chart symlink %q is not supported", path)
		}

		if !entry.Type().IsRegular() {
			return fmt.Errorf("chart entry %q is not a regular file", path)
		}

		return copyFile(path, target)
	})
}

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open chart file %q: %w", source, err)
	}
	defer input.Close() //nolint:errcheck // copy failure is reported separately

	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create chart file %q: %w", target, err)
	}

	if _, err := io.Copy(output, input); err != nil {
		copyErr := fmt.Errorf("copy chart file %q: %w", source, err)
		if closeErr := output.Close(); closeErr != nil {
			return errors.Join(copyErr, fmt.Errorf("close chart file %q: %w", target, closeErr))
		}

		return copyErr
	}

	if err := output.Close(); err != nil {
		return fmt.Errorf("close chart file %q: %w", target, err)
	}

	return nil
}
