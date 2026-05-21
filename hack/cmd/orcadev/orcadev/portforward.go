// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// portForwardProbeTimeout bounds the initial TCP probe used to
// decide whether the orca edge is already reachable on localhost.
// 500 ms is plenty for a loopback dial; longer would slow every
// subcommand startup, shorter risks a false negative on a busy host.
const portForwardProbeTimeout = 500 * time.Millisecond

// portForwardReadyTimeout bounds how long we wait for kubectl to
// print "Forwarding from ..." on stdout after we Start() it. If
// kubectl is going to succeed it does so in well under a second on
// a healthy cluster; the 10s budget covers cold kubeconfig parsing
// and slow API-server contact.
const portForwardReadyTimeout = 10 * time.Second

// orcaEdgeNamespace and orcaEdgeService are hard-coded to the dev
// harness defaults that --orca-url=http://localhost:8443 implies.
// The auto-forward only fires when --orca-url IS the dev default,
// so deriving these from --orca-url would add nothing.
const (
	orcaEdgeNamespace = "unbounded-kube"
	orcaEdgeService   = "orca"
)

// ensureEdgeReachable probes --orca-url; if unreachable AND the URL
// looks like the documented dev default (localhost:8443) AND
// --auto-port-forward is enabled, spawns a kubectl port-forward in
// the background and returns a cleanup that tears it down on
// caller exit. If the URL is already reachable, returns immediately
// with a no-op cleanup.
//
// Caller MUST defer the returned cleanup so the port-forward
// subprocess is stopped on exit. The cleanup is always safe to call,
// even on the no-op path.
//
// Behaviour matrix:
//
//	--auto-port-forward=false           -> no-op cleanup
//	URL host not localhost/127.0.0.1    -> no-op cleanup
//	URL port not 8443                   -> no-op cleanup
//	probe succeeds                      -> no-op cleanup (operator's
//	                                       own port-forward, or any
//	                                       other process bound to the
//	                                       port, is honored)
//	probe fails + everything else true  -> spawn port-forward, return
//	                                       cleanup that SIGTERMs it
func ensureEdgeReachable(ctx context.Context, g *globalFlags) (func(), error) {
	if !g.autoPortForward {
		return func() {}, nil
	}

	host, port, err := hostPortFromURL(g.orcaURL)
	if err != nil {
		// Don't gate on this; the real HTTP call will surface the
		// URL parse error if it actually matters.
		return func() {}, nil
	}

	// Only auto-forward when the user is hitting localhost on the
	// documented edge port. Any other URL means the operator
	// configured a real endpoint and we should stay out of the way.
	if host != "localhost" && host != "127.0.0.1" {
		return func() {}, nil
	}

	if port != "8443" {
		return func() {}, nil
	}

	if probeTCP(host, port, portForwardProbeTimeout) {
		return func() {}, nil
	}

	return spawnPortForward(ctx, g, port)
}

// probeTCP returns true if a TCP connection to host:port completes
// within timeout. The connection is closed immediately on success.
func probeTCP(host, port string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}

	_ = conn.Close() //nolint:errcheck // probe close best-effort

	return true
}

// hostPortFromURL parses u and returns the host and port. Missing
// port is filled from the scheme default (80 for http, 443 for
// https). A parse error is surfaced verbatim.
func hostPortFromURL(u string) (string, string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return "", "", err
	}

	host := parsed.Hostname()

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	return host, port, nil
}

// spawnPortForward starts `kubectl port-forward svc/orca <port>:8443`
// as a subprocess under g.kubeContext and waits up to
// portForwardReadyTimeout for the "Forwarding from" line on stdout.
// Returns a cleanup that SIGTERMs the subprocess and waits for it,
// or an error if the subprocess didn't reach the ready state in
// time.
//
// Stderr is drained into an in-memory buffer so it can be surfaced
// in the error message; we deliberately do NOT stream it to the
// user's stderr during the run (kubectl is chatty about
// "Handling connection for X" lines that would clutter output).
func spawnPortForward(_ context.Context, g *globalFlags, port string) (func(), error) {
	cmd := exec.Command("kubectl",
		"--context", g.kubeContext,
		"-n", orcaEdgeNamespace,
		"port-forward", "svc/"+orcaEdgeService,
		port+":8443",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("port-forward stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("port-forward stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("port-forward start: %w", err)
	}

	// Drain stderr into a buffer so a failing kubectl reports
	// something useful, AND so the subprocess doesn't block on a
	// full pipe if we ever forget to drain it.
	var errBuf strings.Builder

	go func() {
		_, _ = io.Copy(&errBuf, stderr) //nolint:errcheck // drain best-effort
	}()

	ready := make(chan error, 1)

	go func() {
		ready <- waitForForwarding(stdout)
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort
			_ = cmd.Wait()                          //nolint:errcheck // best-effort

			return nil, fmt.Errorf("port-forward did not start: %w (stderr: %s)",
				err, strings.TrimSpace(errBuf.String()))
		}
	case <-time.After(portForwardReadyTimeout):
		_ = cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort
		_ = cmd.Wait()                          //nolint:errcheck // best-effort

		return nil, fmt.Errorf("port-forward timed out after %s (stderr: %s)",
			portForwardReadyTimeout, strings.TrimSpace(errBuf.String()))
	}

	printErr("auto port-forward: localhost:%s -> svc/%s:8443\n", port, orcaEdgeService)

	cleanup := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort
		_ = cmd.Wait()                          //nolint:errcheck // best-effort
	}

	return cleanup, nil
}

// waitForForwarding reads from r until it sees the kubectl
// "Forwarding from" sentinel string or r is closed.
func waitForForwarding(r io.Reader) error {
	const sentinel = "Forwarding from"

	buf := make([]byte, 4096)

	var seen strings.Builder

	for {
		n, err := r.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), sentinel) {
				return nil
			}
		}

		if err != nil {
			return fmt.Errorf("kubectl exited before forwarding: %w; output: %s",
				err, strings.TrimSpace(seen.String()))
		}
	}
}
