// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"k8s.io/client-go/rest"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	allocsPath           = "/apis/playpen.unbounded-cloud.io/v1alpha1/allocs"
	deallocsPath         = "/apis/playpen.unbounded-cloud.io/v1alpha1/deallocs"
)

// Config contains settings for connecting to the playpen aggregated API.
type Config struct {
	// RESTConfig connects to the Kubernetes API server hosting the playpen aggregated API.
	RESTConfig *rest.Config
	// HTTPClient overrides the client built from RESTConfig. It is intended for tests.
	HTTPClient *http.Client
	// Commander overrides local network command execution. It is intended for tests.
	Commander Commander
}

// Client is a high-level client for allocating and releasing playpens.
type Client struct {
	baseURL    string
	httpClient *http.Client
	cmd        Commander
}

// AllocateOptions controls one playpen allocation.
type AllocateOptions struct {
	// Architecture optionally requests a runner architecture. Empty defaults to amd64.
	Architecture string
	// WireGuardPrivateKey is the client's WireGuard private key. If empty, one is generated.
	WireGuardPrivateKey string
	// Tunnel optionally overrides local tunnel settings.
	Tunnel TunnelConfig
}

// Playpen represents one allocated playpen runner pod and its local resources.
type Playpen struct {
	client              *Client
	mu                  sync.Mutex
	idempotencyKey      string
	wireGuardPrivateKey string
	closed              bool
	tunnel              *Tunnel

	Metadata operator.AllocResponse
}

// New returns a client configured to use the Kubernetes aggregated API server.
func New(cfg Config) (*Client, error) {
	if cfg.RESTConfig == nil {
		return nil, fmt.Errorf("REST config is required")
	}

	restConfig := rest.CopyConfig(cfg.RESTConfig)

	baseURL := strings.TrimRight(strings.TrimSpace(restConfig.Host), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("REST config host is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		var err error

		httpClient, err = rest.HTTPClientFor(restConfig)
		if err != nil {
			return nil, fmt.Errorf("create Kubernetes HTTP client: %w", err)
		}
	}

	cmd := cfg.Commander
	if cmd == nil {
		cmd = OSCommander{}
	}

	return &Client{baseURL: baseURL, httpClient: httpClient, cmd: cmd}, nil
}

// Allocate allocates an idle playpen runner and returns its metadata.
func (c *Client) Allocate(ctx context.Context, opts AllocateOptions) (*Playpen, error) {
	privateKey := strings.TrimSpace(opts.WireGuardPrivateKey)
	if privateKey == "" {
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return nil, fmt.Errorf("generate wireguard key: %w", err)
		}

		privateKey = key.String()
	}

	key, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse wireguard private key: %w", err)
	}

	idempotencyKey, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate idempotency key: %w", err)
	}

	body, err := json.Marshal(operator.AllocRequest{WireGuardPublicKey: key.PublicKey().String(), Architecture: opts.Architecture})
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, http.MethodPost, allocsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set(idempotencyKeyHeader, idempotencyKey)
	req.Header.Set("Content-Type", "application/json")

	var resp operator.AllocResponse
	if err := c.doJSON(req, http.StatusOK, &resp); err != nil {
		return nil, err
	}

	return &Playpen{
		client:              c,
		idempotencyKey:      idempotencyKey,
		wireGuardPrivateKey: privateKey,
		Metadata:            resp,
		tunnel: NewTunnel(c.cmd, privateKey, resp, tunnelConfigWithDefaults(
			opts.Tunnel,
			idempotencyKey,
		)),
	}, nil
}

func (c *Client) deallocate(ctx context.Context, idempotencyKey string) error {
	req, err := c.newRequest(ctx, http.MethodPost, deallocsPath, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set(idempotencyKeyHeader, strings.TrimSpace(idempotencyKey))

	return c.doJSON(req, http.StatusNoContent, nil)
}

// WireGuardPrivateKey returns the client's WireGuard private key for this playpen.
func (p *Playpen) WireGuardPrivateKey() string {
	return p.wireGuardPrivateKey
}

// ConfigureTunnel creates the local WireGuard and VXLAN resources for this playpen.
func (p *Playpen) ConfigureTunnel(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("playpen is closed")
	}

	return p.tunnel.Setup(ctx)
}

// Close tears down local tunnel resources and releases the playpen.
func (p *Playpen) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()

		return nil
	}

	p.closed = true
	tunnel := p.tunnel
	idempotencyKey := p.idempotencyKey
	client := p.client
	p.mu.Unlock()

	var closeErr error
	if tunnel != nil {
		closeErr = tunnel.Teardown(ctx)
	}

	if err := client.deallocate(ctx, idempotencyKey); err != nil && closeErr == nil {
		closeErr = err
	}

	return closeErr
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}

	return req, nil
}

func (c *Client) doJSON(req *http.Request, wantStatus int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode != wantStatus {
		data, _ := io.ReadAll(resp.Body) //nolint:errcheck // Best-effort error body read.

		return fmt.Errorf("%s %s returned %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // Best-effort drain of response body.

		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
