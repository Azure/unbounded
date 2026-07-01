// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// StreamConsoleLogs asynchronously streams serial console logs into dst until
// ctx is canceled or the stream fails. The returned channel receives one error.
func (p *Playpen) StreamConsoleLogs(ctx context.Context, dst io.Writer) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.streamConsoleLogs(ctx, dst)
		close(errCh)
	}()

	return errCh
}

func (p *Playpen) streamConsoleLogs(ctx context.Context, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("writer is required")
	}

	streamURL, err := consoleStreamURL(p.Metadata.Redfish)
	if err != nil {
		return err
	}

	header := http.Header{}
	if user := p.Metadata.Redfish["username"]; user != "" {
		header.Set("Authorization", "Basic "+basicAuth(user, p.Metadata.Redfish["password"]))
	}

	conn, _, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{
		HTTPClient: redfishWebSocketHTTPClient(),
		HTTPHeader: header,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow() //nolint:errcheck // Connection cleanup only.

	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary && messageType != websocket.MessageText {
			continue
		}
		if _, err := dst.Write(data); err != nil {
			return err
		}
	}
}

func consoleStreamURL(redfish map[string]string) (string, error) {
	base := strings.TrimRight(redfish["url"], "/")
	path := redfish["serialConsoleStreamURI"]
	if base == "" || path == "" {
		return "", fmt.Errorf("redfish url and serialConsoleStreamURI are required")
	}

	parsed, err := url.Parse(base + path)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported redfish URL scheme %q", parsed.Scheme)
	}

	return parsed.String(), nil
}

func redfishWebSocketHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Redfish is reached through the allocated WireGuard tunnel.
			IdleConnTimeout: 90 * time.Second,
		},
	}
}

func basicAuth(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
