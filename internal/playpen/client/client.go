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
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	allocationsPath      = "/apis/playpen.unbounded-cloud.io/v1alpha1/allocations"
	deallocationsPath    = "/apis/playpen.unbounded-cloud.io/v1alpha1/deallocations"
)

type Config struct {
	RESTConfig *rest.Config
	HTTPClient *http.Client
	cmd        commander
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	cmd        commander
}

type AllocateVMOptions struct {
	Architecture        string
	DiskSize            resource.Quantity
	Memory              resource.Quantity
	CPUs                int
	WireGuardPrivateKey string
	Tunnel              TunnelConfig
}

type Allocation struct {
	client              *Client
	mu                  sync.Mutex
	idempotencyKey      string
	wireGuardPrivateKey string
	closed              bool
	tunnel              *tunnel

	Metadata operator.AllocResponse
}

type PXEServerOptions struct {
	Command string
	Args    []string
	Env     []string
}

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

func (c *Client) AllocateVM(ctx context.Context, opts AllocateVMOptions) (*Allocation, error) {
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

	reqBody := operator.AllocRequest{WireGuardPublicKey: key.PublicKey().String(), Architecture: opts.Architecture, CPUs: opts.CPUs}
	if !opts.DiskSize.IsZero() {
		reqBody.DiskSize = opts.DiskSize.String()
	}

	if !opts.Memory.IsZero() {
		reqBody.Memory = opts.Memory.String()
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, http.MethodPost, allocationsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(idempotencyKeyHeader, idempotencyKey)
	req.Header.Set("Content-Type", "application/json")

	var resp operator.AllocResponse
	if err := c.doJSON(req, http.StatusOK, &resp); err != nil {
		return nil, err
	}

	return &Allocation{
		client:              c,
		idempotencyKey:      idempotencyKey,
		wireGuardPrivateKey: privateKey,
		Metadata:            resp,
		tunnel:              newTunnel(c.cmd, privateKey, resp, tunnelConfigWithDefaults(opts.Tunnel, idempotencyKey)),
	}, nil
}

// Allocate is retained as a convenience alias for existing smoke tests moving to AllocateVM.
func (c *Client) Allocate(ctx context.Context, opts AllocateVMOptions) (*Allocation, error) {
	return c.AllocateVM(ctx, opts)
}

func (c *Client) deallocate(ctx context.Context, allocationID, idempotencyKey string) error {
	body, err := json.Marshal(operator.DeallocRequest{AllocationID: allocationID, IdempotencyKey: idempotencyKey})
	if err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodPost, deallocationsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(idempotencyKeyHeader, strings.TrimSpace(idempotencyKey))
	req.Header.Set("Content-Type", "application/json")

	return c.doJSON(req, http.StatusNoContent, nil)
}

func (a *Allocation) WireGuardPrivateKey() string {
	return a.wireGuardPrivateKey
}

func (a *Allocation) TunnelConfig() TunnelConfig {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.tunnel.cfg
}

func (a *Allocation) ConfigureNetwork(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return fmt.Errorf("allocation is closed")
	}

	return a.tunnel.Setup(ctx)
}

func (a *Allocation) ConfigureTunnel(ctx context.Context) error {
	return a.ConfigureNetwork(ctx)
}

func (a *Allocation) ServePXE(ctx context.Context, opts PXEServerOptions) (*exec.Cmd, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, fmt.Errorf("PXE command is required")
	}

	cmd, err := a.Command(ctx, opts.Command, opts.Args...)
	if err != nil {
		return nil, err
	}

	cmd.Env = append(os.Environ(), opts.Env...)
	cmd.Env = append(cmd.Env,
		"PLAYPEN_GUEST_MAC="+a.Metadata.Network.GuestMAC,
		"PLAYPEN_GUEST_IPV4="+a.Metadata.Network.GuestIPv4,
		"PLAYPEN_GATEWAY_IPV4="+a.Metadata.Network.GatewayIPv4,
	)

	return cmd, cmd.Start()
}

func (a *Allocation) Run(ctx context.Context, name string, args ...string) error {
	cmd, err := a.Command(ctx, name, args...)
	if err != nil {
		return err
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (a *Allocation) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, fmt.Errorf("allocation is closed")
	}

	namespace := strings.TrimSpace(a.tunnel.cfg.NetworkNamespace)
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

func (a *Allocation) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()

		return nil
	}

	a.closed = true
	tunnel := a.tunnel
	idempotencyKey := a.idempotencyKey
	allocationID := a.Metadata.ID
	client := a.client
	a.mu.Unlock()

	var closeErr error
	if tunnel != nil {
		closeErr = tunnel.Teardown(ctx)
	}

	if err := client.deallocate(ctx, allocationID, idempotencyKey); err != nil && closeErr == nil {
		closeErr = err
	}

	return closeErr
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
}

func (c *Client) doJSON(req *http.Request, wantStatus int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != wantStatus {
		data, _ := io.ReadAll(resp.Body) //nolint:errcheck

		return fmt.Errorf("%s %s returned %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck

		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
