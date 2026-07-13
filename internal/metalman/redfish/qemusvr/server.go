// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
)

// execRunner is the default Runner backed by os/exec.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)

	stdout, err := cmd.Output()

	var exit *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exit); ok {
			return string(stdout), exit.ExitCode(), nil
		}

		return string(stdout), -1, err
	}

	return string(stdout), 0, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}

	return ok
}

// Serve builds fixture state and serves the Redfish API over TLS until the
// process is terminated.
func Serve(cfg Config) error {
	state, err := NewState(cfg, nil)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler: state.Handler(),
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	return server.ServeTLS(listener, cfg.Cert, cfg.Key)
}
