// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Azure/unbounded/e2e/racer/fixture"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		s := &http.Server{Addr: ":8080", Handler: fixture.NewOrigin(), ReadHeaderTimeout: 5 * time.Second}
		log.Fatal(s.ListenAndServe())
	}

	if len(os.Args) < 5 || os.Args[1] != "request" {
		log.Fatal("usage: fixture serve | fixture request METHOD URL RANGE [HEADER...]")
	}

	r, err := fixture.Fetch(os.Args[2], os.Args[3], os.Args[4], os.Args[5:]...)
	if err != nil {
		log.Fatal(err)
	}

	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		log.Fatal(err)
	}
}
