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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// portForwardProbeTimeout bounds the initial TCP probe used to decide
// whether a target service is already reachable on localhost (because
// of a kind NodePort mapping, an operator's own `kubectl
// port-forward`, or a sibling orcadev process). 500 ms is plenty for
// a loopback dial; longer would slow every subcommand startup,
// shorter risks a false negative on a busy host.
const portForwardProbeTimeout = 500 * time.Millisecond

// portForwardReadyTimeout bounds how long we wait for kubectl to
// print "Forwarding from ..." on stdout after we Start() it. If
// kubectl is going to succeed it does so in well under a second on
// a healthy cluster; the 10s budget covers cold kubeconfig parsing
// and slow API-server contact.
const portForwardReadyTimeout = 10 * time.Second

// portForwardRetryDelay is the backoff before re-probing localhost
// and re-attempting kubectl after a "bind: address already in use"
// failure. Long enough for a concurrent orcadev's port-forward to
// finish its own startup, short enough to keep the perceived
// latency tolerable.
const portForwardRetryDelay = 500 * time.Millisecond

// portForwardSpec describes one Service to forward. The local-side
// port is bound on 127.0.0.1; the remote-side port is the Service's
// in-cluster port.
type portForwardSpec struct {
	// label is the human-readable name used in startup messages.
	label string
	// service is the Service name in g.namespace.
	service string
	// localPort is the 127.0.0.1 port operators reach via the
	// configured endpoint URL.
	localPort int
	// remotePort is the in-Pod / in-Service port kubectl forwards
	// to.
	remotePort int
}

// ensurePortForwards is the canonical entrypoint every subcommand
// uses. It opens (or reuses) auto port-forwards for all Services
// implied by the resolved global flags - the orca edge, the origin
// emulator (when --origin-endpoint points at localhost), and the
// cachestore emulator (when --cachestore-endpoint points at
// localhost). Each individual forward is independent, lazy
// (TCP-probed first), and cleaned up in reverse order on caller
// exit.
//
// Caller MUST defer the returned cleanup so each subprocess is
// stopped on exit. The cleanup is always safe to call, even on the
// no-op path (when --auto-port-forward=false or every target is
// already bound).
func ensurePortForwards(ctx context.Context, g *globalFlags) (func(), error) {
	if !g.autoPortForward {
		return func() {}, nil
	}

	specs := derivePortForwardSpecs(g)

	cleanups := make([]func(), 0, len(specs))

	rollback := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	for _, spec := range specs {
		cu, err := ensureOnePortForward(ctx, g, spec)
		if err != nil {
			rollback()

			return nil, err
		}

		cleanups = append(cleanups, cu)
	}

	return rollback, nil
}

// ensureEdgeReachable is a thin back-compat wrapper. New code should
// call ensurePortForwards directly. Kept so existing subcommands and
// tests don't churn.
func ensureEdgeReachable(ctx context.Context, g *globalFlags) (func(), error) {
	return ensurePortForwards(ctx, g)
}

// derivePortForwardSpecs inspects the resolved global flags and
// returns the set of port-forwards to maintain for this invocation.
// The order is significant: orca edge first (it's the only one every
// subcommand uses), origin next, cachestore last. Duplicates (e.g.
// origin and cachestore both pointing at LocalStack) are deduped on
// the (service, localPort) pair.
func derivePortForwardSpecs(g *globalFlags) []portForwardSpec {
	specs := []portForwardSpec{}

	seen := map[string]struct{}{}

	add := func(s portForwardSpec) {
		key := s.service + ":" + strconv.Itoa(s.localPort)
		if _, dup := seen[key]; dup {
			return
		}

		seen[key] = struct{}{}
		specs = append(specs, s)
	}

	// Orca edge: forward whenever --orca-url is a localhost URL.
	if host, port, ok := localhostHostPort(g.orcaURL); ok {
		// Only honor the port from the URL; the in-cluster Service
		// always exposes 8443 (it's the deployment contract).
		_ = host

		add(portForwardSpec{
			label:      "orca edge",
			service:    devSvcOrca,
			localPort:  port,
			remotePort: devRemotePortOrca,
		})
	}

	// Origin emulator: only relevant when the operator did not
	// override the endpoint to a real cloud URL.
	if host, port, ok := localhostHostPort(g.originEndpoint); ok {
		_ = host

		switch g.originDriver {
		case "azureblob":
			add(portForwardSpec{
				label:      "azurite (origin)",
				service:    devSvcAzurite,
				localPort:  port,
				remotePort: devRemotePortAzurite,
			})
		case "awss3":
			add(portForwardSpec{
				label:      "localstack (origin)",
				service:    devSvcLocalstack,
				localPort:  port,
				remotePort: devRemotePortLocalstack,
			})
		}
	}

	// Cachestore: always LocalStack-shaped.
	if host, port, ok := localhostHostPort(g.cachestoreEndpoint); ok {
		_ = host

		add(portForwardSpec{
			label:      "localstack (cachestore)",
			service:    devSvcLocalstack,
			localPort:  port,
			remotePort: devRemotePortLocalstack,
		})
	}

	return specs
}

// localhostHostPort parses u and returns the host, port and a bool
// indicating whether the URL is a localhost (or 127.0.0.1) URL we
// should consider for auto-port-forward. Defaults the port from the
// scheme when the URL omits it. A parse error returns false rather
// than surfacing - the real HTTP call will report any URL problem
// more usefully than this probe could.
func localhostHostPort(u string) (host string, port int, ok bool) {
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return "", 0, false
	}

	host = parsed.Hostname()
	if host != "localhost" && host != "127.0.0.1" {
		return "", 0, false
	}

	portStr := parsed.Port()
	if portStr == "" {
		if parsed.Scheme == "https" {
			portStr = "443"
		} else {
			portStr = "80"
		}
	}

	p, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}

	return host, p, true
}

// ensureOnePortForward implements the per-spec contract: probe;
// short-circuit if already bound; otherwise spawn kubectl and
// (carefully) handle the race against a concurrent process binding
// the local port between the probe and our listen.
func ensureOnePortForward(ctx context.Context, g *globalFlags, spec portForwardSpec) (func(), error) {
	if probeTCP("127.0.0.1", strconv.Itoa(spec.localPort), portForwardProbeTimeout) {
		return func() {}, nil
	}

	cleanup, err := spawnPortForward(ctx, g, spec)
	if err == nil {
		return cleanup, nil
	}

	if !isPortInUseError(err) {
		return nil, err
	}

	// A concurrent process likely grabbed the port between our
	// probe and kubectl's listen. Sleep a short jitter and re-
	// probe; if a sibling now serves the port we can ride its
	// forward.
	time.Sleep(portForwardRetryDelay)

	if probeTCP("127.0.0.1", strconv.Itoa(spec.localPort), portForwardProbeTimeout) {
		return func() {}, nil
	}

	return spawnPortForward(ctx, g, spec)
}

// isPortInUseError returns true when err looks like the kubectl
// "Unable to listen on port" / "bind: address already in use"
// failure that occurs when another process holds the local port at
// the moment kubectl attempts to bind. Matched substrings are taken
// verbatim from observed kubectl 1.29 - 1.31 output.
func isPortInUseError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "unable to listen on any of the requested ports")
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
// https). A parse error is surfaced verbatim. Retained for the
// existing unit tests.
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

// portForwardStderrCapacity bounds the in-memory buffer used to
// surface kubectl stderr in failure messages. 16 KiB is more than
// enough for an "Unable to listen on port" stanza yet small enough
// that a multi-hour port-forward leaking spurious stderr (e.g.
// recurring connection-reset warnings) cannot grow without bound.
const portForwardStderrCapacity = 16 * 1024

// spawnPortForward starts `kubectl port-forward svc/<service>
// <localPort>:<remotePort>` as a subprocess under g.kubeContext
// (defaults to the current context when empty) in g.namespace, and
// waits up to portForwardReadyTimeout for the "Forwarding from"
// line on stdout. Returns a cleanup that SIGTERMs the subprocess
// and waits for it, or an error if the subprocess didn't reach the
// ready state in time.
//
// Stderr is drained into a bounded ring buffer so a failing kubectl
// reports something useful; we deliberately do NOT stream it to the
// user's stderr during the run (kubectl is chatty about "Handling
// connection for X" lines that would clutter output).
func spawnPortForward(_ context.Context, g *globalFlags, spec portForwardSpec) (func(), error) {
	args := []string{}

	if g.kubeContext != "" {
		args = append(args, "--context", g.kubeContext)
	}

	namespace := g.namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	args = append(args,
		"-n", namespace,
		"port-forward", "svc/"+spec.service,
		fmt.Sprintf("%d:%d", spec.localPort, spec.remotePort),
	)

	cmd := exec.Command("kubectl", args...)

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

	// Drain stderr into a bounded buffer so the subprocess never
	// blocks on a full pipe and our diagnostic memory footprint is
	// capped regardless of session length.
	errBuf := newRingBuffer(portForwardStderrCapacity)

	go func() {
		_, _ = io.Copy(errBuf, stderr) //nolint:errcheck // drain best-effort
	}()

	ready := make(chan error, 1)

	go func() {
		ready <- waitForForwardingAndDrain(stdout)
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

	printErr("auto port-forward: localhost:%d -> svc/%s:%d (%s)\n",
		spec.localPort, spec.service, spec.remotePort, spec.label)

	cleanup := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort
		_ = cmd.Wait()                          //nolint:errcheck // best-effort
	}

	return cleanup, nil
}

// waitForForwardingAndDrain blocks until r either yields the
// kubectl "Forwarding from" readiness sentinel or hits EOF/an error.
// On success it spawns a fire-and-forget goroutine that continues
// draining r so kubectl can keep writing per-connection log lines
// without blocking on a full stdout pipe.
func waitForForwardingAndDrain(r io.Reader) error {
	drain, err := waitForForwardingReader(r)
	if err != nil {
		return err
	}

	go func() {
		_, _ = io.Copy(io.Discard, drain) //nolint:errcheck // drain best-effort
	}()

	return nil
}

// waitForForwardingReader reads from r until it sees the kubectl
// "Forwarding from" sentinel string and returns r so the caller can
// continue draining post-readiness output, or an error if EOF / a
// read error occurs first.
func waitForForwardingReader(r io.Reader) (io.Reader, error) {
	const sentinel = "Forwarding from"

	buf := make([]byte, 4096)

	var seen strings.Builder

	for {
		n, err := r.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])

			if strings.Contains(seen.String(), sentinel) {
				return r, nil
			}
		}

		if err != nil {
			return nil, fmt.Errorf("kubectl exited before forwarding: %w; output: %s",
				err, strings.TrimSpace(seen.String()))
		}
	}
}
