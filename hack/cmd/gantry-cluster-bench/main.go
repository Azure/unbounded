// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// gantry-cluster-bench is the helper binary for hack/gantry-cluster-bench.
//
// Subcommands:
//
//	gen-image   stream a synthetic N-layer image into an OCI distribution
//	            registry without holding layers in memory or on disk.
//	delete-image delete a manifest (by tag) so registry garbage-collect can
//	            reclaim its blobs.
//	summarize   parse the raw output of run.sh and write summary.md/json.
//	compare     print a side-by-side table of two or more summary.json files.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "gen-image":
		err = runGenImage(os.Args[2:])
	case "delete-image":
		err = runDeleteImage(os.Args[2:])
	case "summarize":
		err = runSummarize(os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gantry-cluster-bench {gen-image|delete-image|summarize|compare} [flags]")
}
