// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

const idempotencyKeyHeader = "Idempotency-Key"

// Config contains settings for connecting to the playpen operator.
type Config struct {
	// BaseURL is the HTTPS base URL of the playpen operator.
	BaseURL string
	// CertFingerprint is the SHA256 fingerprint of the operator TLS certificate.
	CertFingerprint string
	// GitHubActionsOAuthToken is a bearer token issued to a GitHub Action.
	GitHubActionsOAuthToken string
	// KubernetesToken is a bearer token issued by Kubernetes with the "playpen" audience.
	KubernetesToken string
	// HTTPClient overrides the default pinned TLS HTTP client. It is intended for tests.
	HTTPClient *http.Client
	// Commander overrides local network command execution. It is intended for tests.
	Commander Commander
}

// Client is a high-level client for allocating and releasing playpens.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	cmd        Commander
}

// AllocateOptions controls one playpen allocation.
type AllocateOptions struct {
	// IdempotencyKey identifies the allocation. If empty, a random key is generated.
	IdempotencyKey string
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

	Metadata operator.ClaimResponse
}

// New returns a client configured with operator TLS pinning and bearer auth.
func New(cfg Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}

	token, err := bearerToken(cfg)
	if err != nil {
		return nil, err
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		if strings.TrimSpace(cfg.CertFingerprint) == "" {
			return nil, fmt.Errorf("cert fingerprint is required")
		}

		httpClient = newPinnedHTTPClient(normalizeFingerprint(cfg.CertFingerprint))
	}

	cmd := cfg.Commander
	if cmd == nil {
		cmd = OSCommander{}
	}

	return &Client{baseURL: baseURL, token: token, httpClient: httpClient, cmd: cmd}, nil
}

// Allocate claims an idle playpen runner and returns its metadata.
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

	idempotencyKey := strings.TrimSpace(opts.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey, err = randomHex(32)
		if err != nil {
			return nil, fmt.Errorf("generate idempotency key: %w", err)
		}
	}

	body, err := json.Marshal(operator.ClaimRequest{WireGuardPublicKey: key.PublicKey().String()})
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, http.MethodPost, "/playpen/v1/claims", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(idempotencyKeyHeader, idempotencyKey)
	req.Header.Set("Content-Type", "application/json")

	var resp operator.ClaimResponse
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

// Release releases an allocation by idempotency key. It is idempotent server-side.
func (c *Client) Release(ctx context.Context, idempotencyKey string) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/playpen/v1/releases", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set(idempotencyKeyHeader, strings.TrimSpace(idempotencyKey))

	return c.doJSON(req, http.StatusNoContent, nil)
}

// IdempotencyKey returns the key used to allocate this playpen.
func (p *Playpen) IdempotencyKey() string {
	return p.idempotencyKey
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
	if err := client.Release(ctx, idempotencyKey); err != nil && closeErr == nil {
		closeErr = err
	}

	return closeErr
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

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

func bearerToken(cfg Config) (string, error) {
	githubToken := strings.TrimSpace(cfg.GitHubActionsOAuthToken)
	kubernetesToken := strings.TrimSpace(cfg.KubernetesToken)
	switch {
	case githubToken != "" && kubernetesToken != "":
		return "", fmt.Errorf("provide either GitHubActionsOAuthToken or KubernetesToken, not both")
	case githubToken != "":
		return githubToken, nil
	case kubernetesToken != "":
		return kubernetesToken, nil
	default:
		return "", fmt.Errorf("GitHubActionsOAuthToken or KubernetesToken is required")
	}
}

func newPinnedHTTPClient(certFingerprint string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ //nolint:gosec // Certificate trust is enforced by explicit SHA256 pinning below.
				InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return fmt.Errorf("no TLS peer certificates")
					}

					fp := formatFingerprint(sha256Sum(cs.PeerCertificates[0].Raw))
					if fp != certFingerprint {
						return fmt.Errorf("TLS cert SHA256 mismatch: got %s, want %s", fp, certFingerprint)
					}

					return nil
				},
			},
		},
	}
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

func sha256Sum(data []byte) []byte {
	h := sha256.Sum256(data)

	return h[:]
}

func formatFingerprint(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}

	return strings.Join(parts, ":")
}

func normalizeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(value, ":") {
		return value
	}

	if len(value) != sha256.Size*2 {
		return value
	}

	parts := make([]string, sha256.Size)
	for i := range sha256.Size {
		parts[i] = value[i*2 : i*2+2]
	}

	return strings.Join(parts, ":")
}
