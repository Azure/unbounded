// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package moduleclient is the dashboard's HTTP client for talking to component
// modules. It is the concrete transport for the prototype: plain HTTP+JSON
// against a module's base URL. A typed RPC could replace it later without
// changing the dashboard's rendering code.
package moduleclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

// Client talks to a single module's dashboard surface rooted at BaseURL.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client for the given module base URL. A nil httpClient uses a
// default client with a sane timeout.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

// Manifest fetches the module manifest.
func (c *Client) Manifest(ctx context.Context) (*contract.Manifest, error) {
	var m contract.Manifest
	if err := c.getJSON(ctx, "/manifest", &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// Summary fetches the module summary surface.
func (c *Client) Summary(ctx context.Context) (*contract.Summary, error) {
	var s contract.Summary
	if err := c.getJSON(ctx, "/summary", &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// Overview fetches the composed overview surface (ordered panels).
func (c *Client) Overview(ctx context.Context) (*contract.Overview, error) {
	var o contract.Overview
	if err := c.getJSON(ctx, "/overview", &o); err != nil {
		return nil, err
	}

	return &o, nil
}

// Graph fetches the topology graph surface.
func (c *Client) Graph(ctx context.Context) (*contract.Graph, error) {
	var g contract.Graph
	if err := c.getJSON(ctx, "/graph", &g); err != nil {
		return nil, err
	}

	return &g, nil
}

// Matrix fetches the connectivity matrix surface.
func (c *Client) Matrix(ctx context.Context) (*contract.Matrix, error) {
	var m contract.Matrix
	if err := c.getJSON(ctx, "/matrix", &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// Resources fetches the resource list for the given kind.
func (c *Client) Resources(ctx context.Context, kind string) (*contract.ResourceList, error) {
	var rl contract.ResourceList
	if err := c.getJSON(ctx, "/resources/"+url.PathEscape(kind), &rl); err != nil {
		return nil, err
	}

	return &rl, nil
}

// ResourceDetail fetches one resource of the given kind by name.
func (c *Client) ResourceDetail(ctx context.Context, kind, name string) (*contract.ResourceDetail, error) {
	var rd contract.ResourceDetail
	if err := c.getJSON(ctx, "/resources/"+url.PathEscape(kind)+"/"+url.PathEscape(name), &rd); err != nil {
		return nil, err
	}

	return &rd, nil
}

// Invoke executes an action by id with the given form parameters.
func (c *Client) Invoke(ctx context.Context, actionID string, params map[string]string) (*contract.ActionResult, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	endpoint := c.baseURL + "/actions/" + url.PathEscape(actionID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building action request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoking action %q: %w", actionID, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("action %q returned status %d", actionID, resp.StatusCode)
	}

	var result contract.ActionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding action result: %w", err)
	}

	return &result, nil
}

// StreamURL returns the absolute URL of the module's live stream endpoint.
func (c *Client) StreamURL() string {
	return c.baseURL + "/stream"
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	endpoint := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building request for %q: %w", endpoint, err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %q: %w", endpoint, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("module endpoint %q returned status %d", endpoint, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %q: %w", endpoint, err)
	}

	return nil
}
