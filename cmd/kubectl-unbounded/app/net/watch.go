// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// wsMessage is the envelope for WebSocket messages from the controller.
type wsMessage struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message,omitempty"`
}

// clusterStatusDelta is the incremental update sent by the controller.
type clusterStatusDelta struct {
	Seq          uint64              `json:"seq"`
	Timestamp    time.Time           `json:"timestamp"`
	NodeCount    int                 `json:"nodeCount"`
	PullEnabled  bool                `json:"pullEnabled"`
	Warnings     []string            `json:"warnings,omitempty"`
	UpdatedNodes []json.RawMessage   `json:"updatedNodes,omitempty"`
	RemovedNodes []string            `json:"removedNodes,omitempty"`
	Sites        []siteStatus        `json:"sites"`
	GatewayPools []gatewayPoolStatus `json:"gatewayPools"`
}

// watchOpts provides optional configuration for the watch loop.
type watchOpts struct {
	// renderSummary, when non-nil, enables the WS summary protocol. On
	// connect the client sends cluster_summary_subscribe and subsequent
	// updates arrive as cluster_summary messages rendered via this callback.
	// The full-status render callback is still used during HTTP polling
	// fallback because the polling endpoint returns full status.
	renderSummary func(io.Writer, *clusterSummary, bool) error
}

// mergeStatusDelta applies an incremental delta to the current cluster status.
func mergeStatusDelta(current *clusterStatusResponse, deltaRaw json.RawMessage) error {
	var delta clusterStatusDelta
	if err := json.Unmarshal(deltaRaw, &delta); err != nil {
		return fmt.Errorf("unmarshal delta: %w", err)
	}

	current.PullEnabled = delta.PullEnabled
	current.Sites = delta.Sites

	current.GatewayPools = delta.GatewayPools
	if delta.Warnings != nil {
		current.Warnings = delta.Warnings
	}

	// Remove nodes listed in RemovedNodes.
	if len(delta.RemovedNodes) > 0 {
		removed := make(map[string]bool, len(delta.RemovedNodes))
		for _, name := range delta.RemovedNodes {
			removed[name] = true
		}

		filtered := current.Nodes[:0]
		for _, n := range current.Nodes {
			if !removed[n.NodeInfo.Name] {
				filtered = append(filtered, n)
			}
		}

		current.Nodes = filtered
	}

	// Merge updated nodes: shallow-merge JSON fields to preserve unchanged values.
	for _, raw := range delta.UpdatedNodes {
		// Identify node name from nodeInfo.
		var peek struct {
			NodeInfo struct {
				Name string `json:"name"`
			} `json:"nodeInfo"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			return fmt.Errorf("unmarshal updated node identity: %w", err)
		}

		name := peek.NodeInfo.Name
		found := false

		for i, existing := range current.Nodes {
			if strings.EqualFold(existing.NodeInfo.Name, name) {
				// Re-marshal the existing node, then overlay the delta fields.
				base, err := json.Marshal(existing)
				if err != nil {
					return fmt.Errorf("re-marshal existing node %s: %w", name, err)
				}

				merged, err := shallowMergeJSON(base, raw)
				if err != nil {
					return fmt.Errorf("merge node delta %s: %w", name, err)
				}

				var result statusv1alpha1.NodeStatusResponse
				if err := json.Unmarshal(merged, &result); err != nil {
					return fmt.Errorf("unmarshal merged node %s: %w", name, err)
				}

				current.Nodes[i] = result
				found = true

				break
			}
		}

		if !found {
			// New node -- full unmarshal is fine since there is no prior state.
			var newNode statusv1alpha1.NodeStatusResponse
			if err := json.Unmarshal(raw, &newNode); err != nil {
				return fmt.Errorf("unmarshal new node: %w", err)
			}

			current.Nodes = append(current.Nodes, newNode)
		}
	}

	return nil
}

// connectWatchWebSocket dials the controller WebSocket via port-forward to a controller pod.
func connectWatchWebSocket(ctx context.Context, rt *pluginRuntime, ns string, opts nodeStatusFetchOptions) (*websocket.Conn, func(), error) {
	client, err := rt.kubeClient()
	if err != nil {
		return nil, nil, err
	}

	cfg, err := rt.restConfig()
	if err != nil {
		return nil, nil, err
	}

	pods, err := podsForController(ctx, client, ns, opts.controllerDeploy, opts.controllerSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("find controller pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, nil, fmt.Errorf("no controller pods found in namespace %q", ns)
	}

	podName := pods.Items[0].Name

	reqURL := client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(ns).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdyRoundTripperForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build SPDY transport: %w", err)
	}

	localPort, err := findFreePort()
	if err != nil {
		return nil, nil, fmt.Errorf("find free port: %w", err)
	}

	ports := []string{fmt.Sprintf("%d:%s", localPort, opts.controllerPort)}
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})

	fw, err := newPortForwarder(reqURL, transport, upgrader, ports, stopCh, readyCh, io.Discard, io.Discard, []string{"127.0.0.1"})
	if err != nil {
		return nil, nil, fmt.Errorf("create port-forwarder: %w", err)
	}

	errCh := make(chan error, 1)

	go func() { errCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-errCh:
		close(stopCh)
		return nil, nil, fmt.Errorf("port-forward failed: %w", err)
	case <-ctx.Done():
		close(stopCh)
		return nil, nil, ctx.Err()
	}

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/status/ws", localPort)

	// Request an HMAC viewer token for authentication. When port-forwarding
	// directly to the controller pod, the API server front-proxy is bypassed
	// so the controller requires an HMAC token.
	var wsHeaders http.Header
	if viewerToken, tokenErr := requestViewerToken(cfg); tokenErr == nil {
		wsHeaders = http.Header{}
		wsHeaders.Set("Authorization", "Bearer "+viewerToken)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: wsHeaders,
	})
	if err != nil {
		close(stopCh)
		return nil, nil, fmt.Errorf("websocket dial %s: %w", wsURL, err)
	}

	cleanup := func() {
		_ = conn.Close(websocket.StatusNormalClosure, "client closing") //nolint:errcheck

		close(stopCh)
	}

	return conn, cleanup, nil
}

// findFreePort returns an available TCP port.
func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port //nolint:errcheck
	_ = l.Close()                        //nolint:errcheck

	return port, nil
}

// renderWatchScreenCore contains the shared terminal rendering logic for the
// watch TUI. The leaderPod string is displayed in the header, renderBody
// writes the main table content, and warnings are shown below the table.
func renderWatchScreenCore(
	stdout *os.File,
	leaderPod string,
	connState string,
	lastSeq uint64,
	lastUpdate time.Time,
	useColor bool,
	renderBody func(io.Writer) error,
	warnings []string,
) error {
	// Clear screen and reposition cursor.
	_, _ = fmt.Fprint(stdout, "\033[2J\033[H") //nolint:errcheck

	// Wrap stdout to convert \n to \r\n for raw terminal mode.
	crlf := &crlfWriter{w: stdout}

	// Header: title (left), connection state (center), leader + time (right).
	title := "Unbounded CNI Status - Nodes"
	now := time.Now().Format("2006-01-02 15:04:05")

	rightSide := ""
	if leaderPod != "" {
		rightSide = leaderPod + "  "
	}

	rightSide += now

	// Build the connection state indicator.
	var stateIndicator string

	switch connState {
	case "Live":
		if useColor {
			stateIndicator = "* \033[32mLive\033[0m"
		} else {
			stateIndicator = "* Live"
		}
	case "Polling":
		if useColor {
			stateIndicator = "* \033[33mPolling\033[0m"
		} else {
			stateIndicator = "* Polling"
		}
	default:
		if useColor {
			stateIndicator = "* \033[31mDisconnected\033[0m"
		} else {
			stateIndicator = "* Disconnected"
		}
	}
	// Plain-text length for padding calculation (excludes ANSI codes).
	stateTextLen := len("* ") + len(connState)

	if tw, _, sizeErr := term.GetSize(int(stdout.Fd())); sizeErr == nil && tw > 0 {
		totalFixed := len(title) + stateTextLen + len(rightSide)
		remaining := tw - totalFixed
		leftPad := remaining / 2

		rightPad := remaining - leftPad
		if leftPad < 1 {
			leftPad = 1
		}

		if rightPad < 1 {
			rightPad = 1
		}

		if useColor {
			_, _ = fmt.Fprintf(crlf, "\033[1m%s\033[0m%s%s%s\033[2m%s\033[0m\n", //nolint:errcheck
				title, strings.Repeat(" ", leftPad), stateIndicator,
				strings.Repeat(" ", rightPad), rightSide)
		} else {
			_, _ = fmt.Fprintf(crlf, "%s%s%s%s%s\n", //nolint:errcheck
				title, strings.Repeat(" ", leftPad), stateIndicator,
				strings.Repeat(" ", rightPad), rightSide)
		}
	} else {
		_, _ = fmt.Fprintf(crlf, "%s  %s  %s\n", title, stateIndicator, rightSide) //nolint:errcheck
	}

	_, _ = fmt.Fprintln(crlf) //nolint:errcheck

	// Body table.
	if err := renderBody(crlf); err != nil {
		return err
	}

	// Warnings (after table).
	if len(warnings) > 0 {
		_, _ = fmt.Fprintln(crlf) //nolint:errcheck
		printWarnings(crlf, warnings, useColor)
	}

	// Instructions.
	ago := time.Since(lastUpdate).Truncate(time.Second)
	modeLabel := "live"

	switch connState {
	case "Polling":
		modeLabel = "polling"
	case "Disconnected":
		modeLabel = "disconnected"
	}

	instrLine := fmt.Sprintf("\nWatching %s (seq %d, updated %s ago) -- press q or Ctrl-C to exit", modeLabel, lastSeq, ago)
	if useColor {
		instrLine = "\033[2m" + instrLine + "\033[0m"
	}

	_, _ = fmt.Fprintln(crlf, instrLine) //nolint:errcheck

	// Park cursor at bottom-right so it doesn't distract.
	if tw, th, sizeErr := term.GetSize(int(stdout.Fd())); sizeErr == nil {
		_, _ = fmt.Fprintf(stdout, "\033[%d;%dH", th, tw) //nolint:errcheck
	}

	return nil
}

// renderWatchScreen clears the terminal and renders the current cluster status
// with a 3-column header showing title, connection state, and leader + time.
func renderWatchScreen(
	w io.Writer,
	stdout *os.File,
	current clusterStatusResponse,
	connState string,
	lastSeq uint64,
	lastUpdate time.Time,
	useColor bool,
	render func(io.Writer, clusterStatusResponse, bool) error,
) error {
	leaderPod := ""
	if current.LeaderInfo != nil && current.LeaderInfo.PodName != "" {
		leaderPod = current.LeaderInfo.PodName
	}

	return renderWatchScreenCore(stdout, leaderPod, connState, lastSeq, lastUpdate, useColor,
		func(w io.Writer) error { return render(w, current, useColor) },
		collectWarnings(current))
}

// renderWatchScreenSummary renders the watch TUI from a cluster summary. It
// uses the same screen layout as renderWatchScreen but extracts header and
// warning data from the lightweight summary instead of full cluster status.
func renderWatchScreenSummary(
	stdout *os.File,
	summary *clusterSummary,
	connState string,
	lastSeq uint64,
	lastUpdate time.Time,
	useColor bool,
	render func(io.Writer, *clusterSummary, bool) error,
) error {
	leaderPod := ""
	if summary.LeaderInfo != nil && summary.LeaderInfo.PodName != "" {
		leaderPod = summary.LeaderInfo.PodName
	}

	return renderWatchScreenCore(stdout, leaderPod, connState, lastSeq, lastUpdate, useColor,
		func(w io.Writer) error { return render(w, summary, useColor) },
		collectWarningsFromSummary(*summary))
}

// runWatch connects via WebSocket and renders live-updating data to the terminal.
// On WebSocket failure it reconnects with exponential backoff and falls back to
// HTTP polling via fetchClusterStatus while disconnected.
//
// When wopts is non-nil and wopts.renderSummary is set, the client subscribes
// to the lightweight cluster_summary protocol. If the controller supports it,
// subsequent updates arrive as cluster_summary messages and are rendered via
// renderSummary. If the controller is older and does not support the protocol,
// the subscribe message is silently ignored and the client falls back to
// cluster_status / cluster_status_delta handled by the render callback.
func runWatch(
	ctx context.Context,
	rt *pluginRuntime,
	cmd *cobra.Command,
	fetchOpts nodeStatusFetchOptions,
	colorMode string,
	render func(w io.Writer, status clusterStatusResponse, useColor bool) error,
	wopts *watchOpts,
) error {
	ns, err := rt.namespace()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Enter raw mode for 'q' keypress detection. Cleanup is in the main
	// function scope so the terminal is always restored, even on error exits.
	var terminalRestored bool

	fd := int(os.Stdin.Fd())

	isTTY := term.IsTerminal(fd)
	if isTTY {
		// Enter alternate screen buffer (like `watch` does) so the previous
		// terminal content is restored when we exit.
		_, _ = fmt.Fprint(os.Stdout, "\033[?1049h") //nolint:errcheck

		oldState, stateErr := term.GetState(fd)
		if stateErr == nil {
			if _, rawErr := term.MakeRaw(fd); rawErr == nil {
				defer func() {
					if !terminalRestored {
						_ = term.Restore(fd, oldState)              //nolint:errcheck
						_, _ = fmt.Fprint(os.Stdout, "\033[?1049l") //nolint:errcheck
					}
				}()

				go func() {
					<-ctx.Done()

					terminalRestored = true
					_ = term.Restore(fd, oldState)              //nolint:errcheck
					_, _ = fmt.Fprint(os.Stdout, "\033[?1049l") //nolint:errcheck
				}()
			}
		}
	}

	// Listen for 'q' keypress to quit.
	go func() {
		buf := make([]byte, 1)
		for {
			n, readErr := os.Stdin.Read(buf)
			if readErr != nil || n == 0 {
				return
			}

			if buf[0] == 'q' || buf[0] == 'Q' || buf[0] == 3 { // 3 = Ctrl-C
				cancel()
				return
			}
		}
	}()

	useColor := shouldUseColor(os.Stdout, colorMode)

	var (
		current    clusterStatusResponse
		lastSeq    uint64
		lastUpdate time.Time
	)

	initialized := false

	var connState string

	// Summary protocol state.
	summaryMode := wopts != nil && wopts.renderSummary != nil

	var currentSummary *clusterSummary

	summaryInitialized := false

	const (
		backoffMin   = 1 * time.Second
		backoffMax   = 30 * time.Second
		pollInterval = 5 * time.Second
	)

	backoff := backoffMin

	for {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(os.Stdout) //nolint:errcheck
			return nil
		}

		// Attempt WebSocket connection.
		conn, cleanup, connErr := connectWatchWebSocket(ctx, rt, ns, fetchOpts)
		if connErr != nil {
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(os.Stdout) //nolint:errcheck
				return nil
			}
			// WebSocket connection failed -- fall through to polling.
			connState = "Polling"
		} else {
			// WebSocket connected successfully.
			connState = "Live"
			backoff = backoffMin

			conn.SetReadLimit(32 * 1024 * 1024)

			// Subscribe to the summary protocol when configured. If the
			// controller does not support it the message is silently ignored
			// and the client keeps receiving cluster_status / cluster_status_delta.
			if summaryMode {
				sub, _ := json.Marshal(wsClientMessage{Type: "cluster_summary_subscribe"}) //nolint:errcheck
				_ = conn.Write(ctx, websocket.MessageText, sub)                            //nolint:errcheck
			}

			// Inner read loop: process messages until an error occurs.
			for {
				_, data, readErr := conn.Read(ctx)
				if readErr != nil {
					cleanup()

					if ctx.Err() != nil {
						_, _ = fmt.Fprintln(os.Stdout) //nolint:errcheck
						return nil
					}

					connState = "Polling"

					break
				}

				var msg wsMessage
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}

				switch msg.Type {
				case "cluster_status":
					if err := json.Unmarshal(msg.Data, &current); err != nil {
						continue
					}

					initialized = true
					lastUpdate = time.Now()
				case "cluster_status_delta":
					if !initialized {
						continue
					}

					if err := mergeStatusDelta(&current, msg.Data); err != nil {
						continue
					}

					var seqPeek struct {
						Seq uint64 `json:"seq"`
					}

					_ = json.Unmarshal(msg.Data, &seqPeek) //nolint:errcheck
					lastSeq = seqPeek.Seq
					lastUpdate = time.Now()
				case "cluster_summary":
					if !summaryMode {
						continue
					}

					var summary clusterSummary
					if err := json.Unmarshal(msg.Data, &summary); err != nil {
						continue
					}

					currentSummary = &summary
					summaryInitialized = true
					lastSeq = summary.Seq
					lastUpdate = time.Now()
				default:
					continue
				}

				// Render using the best available data source: prefer the
				// summary when available (lightweight), fall back to full status.
				switch {
				case summaryInitialized && summaryMode:
					if err := renderWatchScreenSummary(os.Stdout, currentSummary, connState,
						lastSeq, lastUpdate, useColor, wopts.renderSummary); err != nil {
						return err
					}
				case initialized:
					if err := renderWatchScreen(os.Stdout, os.Stdout, current, connState,
						lastSeq, lastUpdate, useColor, render); err != nil {
						return err
					}
				}
			}
		}

		// Polling fallback while waiting to reconnect WebSocket.
		// Poll once before applying the backoff wait.
		pollStatus, pollErr := fetchClusterStatus(rt, cmd, fetchOpts)
		if pollErr == nil {
			current = pollStatus
			initialized = true
			lastUpdate = time.Now()
			connState = "Polling"
		} else {
			if !initialized {
				connState = "Disconnected"
			}
			// Keep connState as "Polling" if we had data before.
		}

		if initialized {
			if err := renderWatchScreen(os.Stdout, os.Stdout, current, connState,
				lastSeq, lastUpdate, useColor, render); err != nil {
				return err
			}
		}

		// Backoff wait before attempting WebSocket reconnection.
		// While waiting, continue polling every pollInterval.
		waited := time.Duration(0)
		for waited < backoff {
			sleepDur := pollInterval
			if remaining := backoff - waited; remaining < sleepDur {
				sleepDur = remaining
			}

			select {
			case <-ctx.Done():
				_, _ = fmt.Fprintln(os.Stdout) //nolint:errcheck
				return nil
			case <-time.After(sleepDur):
			}

			waited += sleepDur

			// Poll during the backoff window.
			pollStatus, pollErr := fetchClusterStatus(rt, cmd, fetchOpts)
			if pollErr == nil {
				current = pollStatus
				initialized = true
				lastUpdate = time.Now()
				connState = "Polling"
			} else if !initialized {
				connState = "Disconnected"
			}

			if initialized {
				if err := renderWatchScreen(os.Stdout, os.Stdout, current, connState,
					lastSeq, lastUpdate, useColor, render); err != nil {
					return err
				}
			}
		}

		// Exponential backoff: double the wait, capped at backoffMax.
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// shallowMergeJSON overlays delta JSON fields onto base JSON, returning merged JSON.
func shallowMergeJSON(base, overlay []byte) ([]byte, error) {
	var baseMap, overlayMap map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return overlay, nil
	}

	if err := json.Unmarshal(overlay, &overlayMap); err != nil {
		return base, nil
	}

	for k, v := range overlayMap {
		baseMap[k] = v
	}

	return json.Marshal(baseMap)
}

// crlfWriter wraps a writer and converts lone \n to \r\n for raw terminal mode.
type crlfWriter struct {
	w io.Writer
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	var written int

	for len(p) > 0 {
		i := 0
		for i < len(p) && p[i] != '\n' {
			i++
		}

		if i > 0 {
			n, err := c.w.Write(p[:i])

			written += n
			if err != nil {
				return written, err
			}

			p = p[i:]
		}

		if len(p) > 0 && p[0] == '\n' {
			n, err := c.w.Write([]byte("\r\n"))
			if err != nil {
				return written, err
			}

			_ = n
			written++
			p = p[1:]
		}
	}

	return written, nil
}
