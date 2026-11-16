// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command render-manifests writes the plugin's deployment manifests,
// including the Karpenter CRDs of the pinned upstream module.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Azure/unbounded/plugins/karpenter/internal/manifests"
)

func main() {
	var (
		options manifests.Options
		output  string
	)

	flag.StringVar(&options.Namespace, "namespace", "unbounded-system", "Controller namespace")
	flag.StringVar(&options.Image, "image", "", "Explicit controller image reference")
	flag.StringVar(&options.PriceCurrency, "price-currency", "USD", "ISO 4217 currency whose offering prices are used")
	flag.StringVar(&output, "output", "-", "Output file, or - for standard output")
	flag.Parse()

	if err := run(options, output, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "render manifests: %v\n", err)
		os.Exit(1)
	}
}

func run(options manifests.Options, output string, stdout io.Writer) error {
	content, err := manifests.Render(options)
	if err != nil {
		return err
	}

	if output == "-" {
		if _, err := stdout.Write(content); err != nil {
			return fmt.Errorf("write manifests: %w", err)
		}

		return nil
	}

	if err := os.WriteFile(output, content, 0o600); err != nil {
		return fmt.Errorf("write manifests: %w", err)
	}

	return nil
}
