// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Azure/unbounded/hack/cmd/agent-artifacts-builder/artifacts"
)

type stringList []string

func (s *stringList) String() string {
	items := append([]string(nil), *s...)
	sort.Strings(items)
	return strings.Join(items, ",")
}

func (s *stringList) Set(v string) error {
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		*s = append(*s, item)
	}

	return nil
}

func main() {
	var opts artifacts.Options
	var arches stringList

	flag.StringVar(&opts.OutputDir, "output-dir", "", "Directory where the offline artifact filesystem layout is written")
	flag.StringVar(&opts.OCIRef, "oci-ref", "", "Optional OCI artifact reference to push, with or without oci:// prefix")
	flag.StringVar(&opts.ManifestPath, "manifest", "", "Path to offline artifact manifest.json declaring artifact versions")
	flag.Var(&arches, "arch", "Target architecture to include. Repeat or comma separate. Defaults to the host GOARCH")
	flag.BoolVar(&opts.SkipExisting, "skip-existing", false, "Reuse existing files in output dir instead of downloading them again")
	flag.Parse()

	opts.Architectures = arches
	if opts.ManifestPath == "" {
		exitWithError("--manifest is required")
	}

	if err := artifacts.Build(context.Background(), opts); err != nil {
		exitWithError(err.Error())
	}
}

func exitWithError(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
