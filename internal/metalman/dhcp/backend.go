// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dhcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"strings"
)

// HTTPDecisionProvider asks the Metalman server for an immutable session DHCP
// decision. It has no Kubernetes or OCI cache dependency.
type HTTPDecisionProvider struct {
	backendURL *url.URL
	endpoint   string
	token      string
	tokenFile  string
	client     *http.Client
}

func NewHTTPDecisionProviderFromTokenFile(backendURL, endpoint, tokenFile string, client *http.Client) (*HTTPDecisionProvider, error) {
	if strings.TrimSpace(tokenFile) == "" {
		return nil, errors.New("edge authentication token file is required")
	}

	provider, err := newHTTPDecisionProvider(backendURL, endpoint, client)
	if err != nil {
		return nil, err
	}

	provider.tokenFile = tokenFile

	return provider, nil
}

func NewHTTPDecisionProvider(backendURL, endpoint, token string, client *http.Client) (*HTTPDecisionProvider, error) {
	provider, err := newHTTPDecisionProvider(backendURL, endpoint, client)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(token) == "" {
		return nil, errors.New("edge authentication token is required")
	}

	provider.token = token

	return provider, nil
}

func newHTTPDecisionProvider(backendURL, endpoint string, client *http.Client) (*HTTPDecisionProvider, error) {
	backend, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("parsing backend URL: %w", err)
	}

	if (backend.Scheme != "http" && backend.Scheme != "https") || backend.Host == "" {
		return nil, errors.New("backend URL must use HTTP or HTTPS and include a host")
	}

	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("netboot endpoint name is required")
	}

	if client == nil {
		client = http.DefaultClient
	}

	return &HTTPDecisionProvider{backendURL: backend, endpoint: endpoint, client: client}, nil
}

func (p *HTTPDecisionProvider) Decide(ctx context.Context, mac string, httpClient bool) (*Decision, error) {
	requestURL := *p.backendURL
	requestURL.Path = pathpkg.Join(requestURL.Path, "v1/netboot/endpoints", p.endpoint, "dhcp", mac)
	query := requestURL.Query()
	query.Set("httpClient", fmt.Sprintf("%t", httpClient))
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating DHCP decision request: %w", err)
	}

	token := p.token
	if p.tokenFile != "" {
		tokenBytes, err := os.ReadFile(p.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading edge authentication token: %w", err)
		}

		token = strings.TrimSpace(string(tokenBytes))
	}

	request.Header.Set("Authorization", "Bearer "+token)

	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("requesting DHCP decision: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // Response body is discarded after decoding.

	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DHCP decision server returned %s", response.Status)
	}

	var decision Decision
	if err := json.NewDecoder(response.Body).Decode(&decision); err != nil {
		return nil, fmt.Errorf("decoding DHCP decision: %w", err)
	}

	return &decision, nil
}
