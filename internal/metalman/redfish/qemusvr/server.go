// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
)

// Serve wires the QEMU machine layer to the Redfish server and serves the API
// over TLS until the process is terminated.
func Serve(cfg Config) error {
	machine, err := NewMachine(cfg)
	if err != nil {
		return err
	}

	server, err := NewServer(cfg, machine)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: server.Handler(),
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	return httpServer.ServeTLS(listener, cfg.Cert, cfg.Key)
}
