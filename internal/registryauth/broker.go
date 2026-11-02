// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Broker serves only the infrastructure identity. Application ServiceAccounts
// and their tokens never participate in this node-wide credential flow.
type Broker struct {
	mu         sync.RWMutex
	provider   *Provider
	registries []string
}

// Configure atomically publishes a validated identity and registry allowlist.
// A nil provider revokes broker service while configuration is absent or invalid.
func (b *Broker) Configure(cfg Config, provider *Provider) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.provider = provider

	b.registries = append([]string(nil), cfg.Registries...)
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/credentials" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)

		return
	}

	b.mu.RLock()
	provider, registries := b.provider, append([]string(nil), b.registries...)
	b.mu.RUnlock()

	if provider == nil {
		http.Error(w, "registry broker is not configured; inspect node-auth readiness", http.StatusServiceUnavailable)
		return
	}

	var response bytes.Buffer
	if err := provider.Run(r.Context(), r.Body, &response, registries); err != nil {
		// Provider errors are deliberately credential-free; never include an
		// upstream response body or the caller's raw request in diagnostics.
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response.Bytes()) //nolint:errcheck // The kubelet retries a disconnected credential request.
}

// RunExec implements kubelet's stdio v1 protocol by forwarding to the protected
// node-local broker. It has no Azure identity, token file, or IMDS fallback.
func RunExec(ctx context.Context, input io.Reader, output io.Writer, socket string, registries []string) error {
	data, err := io.ReadAll(io.LimitReader(input, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("credential-provider input exceeds limit or is unreadable")
	}

	var request struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Image      string `json:"image"`
	}
	if json.Unmarshal(data, &request) != nil || request.Kind != "CredentialProviderRequest" || request.APIVersion != providerVersion {
		return errors.New("unsupported or malformed credential-provider request")
	}

	host, _, _, parseErr := ParseImage(request.Image)

	matched := false
	for _, registry := range registries {
		matched = matched || registry == host
	}

	if parseErr != nil || !matched {
		return json.NewEncoder(output).Encode(ProviderResponse{Kind: "CredentialProviderResponse", APIVersion: providerVersion, CacheKeyType: "Registry", CacheDuration: "0s", Auth: map[string]Credential{}})
	}
	// Re-encode only the fields needed by this infrastructure broker. In
	// particular, do not forward any application ServiceAccount token.
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://registry-auth/credentials", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	response, err := client.Do(req)
	if err != nil {
		return errors.New("node-local registry broker is unavailable; inspect gantry-acr-auth readiness")
	}
	defer closeQuietly(response.Body)

	if response.StatusCode != http.StatusOK {
		return errors.New("node-local registry broker could not obtain credentials; inspect gantry-acr-auth logs")
	}

	data, err = io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("invalid registry broker response size")
	}

	var wire ProviderResponse
	if json.Unmarshal(data, &wire) != nil || wire.Kind != "CredentialProviderResponse" || wire.APIVersion != providerVersion {
		return errors.New("invalid registry broker protocol response")
	}

	for registry := range wire.Auth {
		if registry != host {
			return errors.New("registry broker returned credentials for an unexpected destination")
		}
	}

	_, err = output.Write(data)

	return err
}

// ListenBroker claims only a marked root-only runtime directory. A live socket,
// foreign file, or symlink is never replaced. No credentials are stored here.
func ListenBroker(root string) (net.Listener, error) {
	socket := filepath.Join(root, BrokerSocketPath)

	installer := &Installer{Root: root}
	if err := installer.checkPath(BrokerSocketPath); err != nil {
		return nil, err
	}

	dir := filepath.Dir(socket)

	marker := filepath.Join(dir, ".owner")
	if err := installer.checkPath(filepath.Join(filepath.Dir(BrokerSocketPath), ".owner")); err != nil {
		return nil, err
	}

	owner, err := os.ReadFile(marker)
	if err == nil {
		if string(owner) != managedMarker {
			return nil, errors.New("registry broker runtime directory has an unknown owner")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if entries, listErr := os.ReadDir(dir); listErr == nil && len(entries) > 0 {
			return nil, errors.New("refusing to take over an unowned registry broker runtime directory")
		} else if listErr != nil && !errors.Is(listErr, os.ErrNotExist) {
			return nil, listErr
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}

		file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}

		_, writeErr := file.WriteString(managedMarker)
		closeErr := file.Close()

		if writeErr != nil {
			return nil, writeErr
		}

		if closeErr != nil {
			return nil, closeErr
		}
	} else {
		return nil, err
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}

	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket registry broker path")
		}

		connection, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr == nil {
			closeQuietly(connection)
			return nil, errors.New("another registry broker is already listening on this node")
		}

		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, errors.New("could not establish whether the registry broker socket is stale")
		}

		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(socket, 0o600); err != nil {
		closeQuietly(listener)
		return nil, err
	}

	return listener, nil
}
