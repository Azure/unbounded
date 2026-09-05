// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/rest"

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
	cmd        commander
}

// Client is a high-level client for allocating and releasing playpens.
type Client struct {
	baseURL    string
	httpClient *http.Client
	cmd        commander
}

// AllocateOptions controls one playpen allocation.
type AllocateOptions struct {
	// Architecture optionally requests a runner architecture. Empty defaults to amd64.
	Architecture string
	// KubernetesVersion optionally requests a control-plane Kubernetes version.
	KubernetesVersion string
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
	tunnel              *tunnel

	Metadata operator.AllocResponse
}

// ControlPlane represents one allocated playpen Kubernetes control plane.
type ControlPlane struct {
	client         *Client
	mu             sync.Mutex
	idempotencyKey string
	closed         bool

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

	cmd := cfg.cmd
	if cmd == nil {
		cmd = osCommander{}
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
		tunnel: newTunnel(c.cmd, privateKey, resp, tunnelConfigWithDefaults(
			opts.Tunnel,
			idempotencyKey,
		)),
	}, nil
}

// AllocateControlPlane allocates an idle Kubernetes control plane and returns its metadata.
func (c *Client) AllocateControlPlane(ctx context.Context, opts AllocateOptions) (*ControlPlane, error) {
	idempotencyKey, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate idempotency key: %w", err)
	}

	body, err := json.Marshal(operator.AllocRequest{ResourceType: operator.ResourceTypeControlPlane, KubernetesVersion: opts.KubernetesVersion})
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

	return &ControlPlane{client: c, idempotencyKey: idempotencyKey, Metadata: resp}, nil
}

func (c *Client) deallocate(ctx context.Context, idempotencyKey string) error {
	req, err := c.newRequest(ctx, http.MethodPost, deallocsPath, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set(idempotencyKeyHeader, strings.TrimSpace(idempotencyKey))

	return c.doJSON(req, http.StatusNoContent, nil)
}

// TunnelConfig returns the local tunnel settings for this playpen.
func (p *Playpen) TunnelConfig() TunnelConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.tunnel.cfg
}

// OverrideEndpoint replaces the WireGuard endpoint used by future tunnel setup.
func (p *Playpen) OverrideEndpoint(host string, port int32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Metadata.Endpoint.Host = strings.TrimSpace(host)
	p.Metadata.Endpoint.WireGuardUDPPort = port
	p.tunnel.metadata.Endpoint.Host = p.Metadata.Endpoint.Host
	p.tunnel.metadata.Endpoint.WireGuardUDPPort = p.Metadata.Endpoint.WireGuardUDPPort
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

// Run executes a command inside the playpen network namespace.
func (p *Playpen) Run(ctx context.Context, name string, args ...string) error {
	cmd, err := p.Command(ctx, name, args...)
	if err != nil {
		return err
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// Command returns a command configured to execute inside the playpen network namespace.
func (p *Playpen) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("playpen is closed")
	}

	namespace := strings.TrimSpace(p.tunnel.cfg.NetworkNamespace)
	if namespace == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	cmdArgs := append([]string{"netns", "exec", namespace, name}, args...)

	cmdName := "ip"
	if os.Geteuid() != 0 {
		cmdArgs = append([]string{"-n", cmdName}, cmdArgs...)
		cmdName = "sudo"
	}

	return exec.CommandContext(ctx, cmdName, cmdArgs...), nil
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

// Kubeconfig returns the host-reachable kubeconfig for this control plane.
func (cp *ControlPlane) Kubeconfig() string {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	return cp.Metadata.ControlPlane.Kubeconfig
}

// Close releases the control-plane allocation.
func (cp *ControlPlane) Close(ctx context.Context) error {
	cp.mu.Lock()
	if cp.closed {
		cp.mu.Unlock()

		return nil
	}

	cp.closed = true
	idempotencyKey := cp.idempotencyKey
	client := cp.client
	cp.mu.Unlock()

	return client.deallocate(ctx, idempotencyKey)
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
