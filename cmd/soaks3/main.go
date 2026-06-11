// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command soaks3 is an S3 load generator for benchmarking an
// unbounded-storage cluster. It provides two subcommands: "seed" generates
// deterministic test data onto the local filesystem (to be uploaded to an
// origin bucket out of band), and "run" drives read load against an
// unbounded-storage S3 frontend.
package main

import "github.com/Azure/unbounded/cmd/soaks3/app"

func main() {
	app.Run()
}
