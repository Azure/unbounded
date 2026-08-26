// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// render-manifests is a generic Go template renderer. It walks --templates-dir
// for *.yaml.tmpl files, executes each with Go's text/template (plus the sprig
// function library), and writes the rendered output under --output-dir
// mirroring the source tree structure.
//
// Template data is supplied via repeatable --set key=value flags. Missing keys
// evaluate to empty strings (text/template's missingkey=zero behavior for map
// data), which lets templates rely on sprig's `default` function to supply
// documented fallbacks.
//
// The actual rendering logic lives in the render sub-package so it can be
// invoked programmatically from tests.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// setFlags implements flag.Value for repeatable --set key=value arguments.
type setFlags map[string]string

func (s setFlags) String() string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+s[k])
	}

	return strings.Join(pairs, ",")
}

func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--set value must be key=value, got %q", v)
	}

	s[k] = val

	return nil
}

func main() {
	var (
		templatesDir string
		outputDir    string
		data         = setFlags{}
	)

	flag.StringVar(&templatesDir, "templates-dir", "", "Directory containing *.yaml.tmpl manifest templates")
	flag.StringVar(&outputDir, "output-dir", "", "Directory where rendered manifests are written")
	flag.Var(data, "set", "Template variable as key=value (repeatable)")
	flag.Parse()

	if templatesDir == "" {
		exitWithError("--templates-dir is required")
	}

	if outputDir == "" {
		exitWithError("--output-dir is required")
	}

	if err := render.Render(templatesDir, outputDir, data); err != nil {
		exitWithError(err.Error())
	}
}

func exitWithError(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
