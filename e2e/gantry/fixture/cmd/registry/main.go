// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Azure/unbounded/e2e/gantry/fixture"
)

func main() {
	listen := flag.String("listen", ":5443", "HTTPS listen address")
	certificate := flag.String("cert", "/tls/tls.crt", "TLS certificate path")
	key := flag.String("key", "/tls/tls.key", "TLS private key path")

	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	server := &http.Server{
		Addr:              *listen,
		Handler:           fixture.Registry(logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServeTLS(*certificate, *key); err != nil {
		logger.Error("fixture registry stopped", "err", err)
		os.Exit(1)
	}
}
