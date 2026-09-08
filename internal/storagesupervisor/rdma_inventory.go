// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package storagesupervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	storageRdmaHcasAnnotation     = "storage.unbounded-cloud.io/rdma-hcas"
	storageBlockDevicesAnnotation = "storage.unbounded-cloud.io/block-devices"
	deviceInventoryPollInterval   = 30 * time.Second
	deviceInventoryHTTPTimeout    = 5 * time.Second
)

type inventoryEndpoint struct {
	annotation string
	url        string
}

type deviceInventoryPublisher struct {
	clientset  kubernetes.Interface
	nodeName   string
	baseURL    string
	endpoints  []inventoryEndpoint
	interval   time.Duration
	httpClient *http.Client
	signal     func()
}

func startDeviceInventoryPublisher(ctx context.Context, cfg Config, cs kubernetes.Interface, signal func()) {
	if cfg.DeviceInventoryURL == "" {
		return
	}

	if cfg.NodeName == "" {
		slog.Warn("device inventory publishing disabled: NODE_NAME is not set", "url", cfg.DeviceInventoryURL)

		return
	}

	baseURL := strings.TrimRight(cfg.DeviceInventoryURL, "/")

	rdmaURL, err := inventoryURLForPath(baseURL, "/rdma")
	if err != nil {
		slog.Warn("device inventory publishing disabled: invalid URL", "url", cfg.DeviceInventoryURL, "error", err)

		return
	}

	blockURL, err := inventoryURLForPath(baseURL, "/block")
	if err != nil {
		slog.Warn("device inventory publishing disabled: invalid URL", "url", cfg.DeviceInventoryURL, "error", err)

		return
	}

	p := &deviceInventoryPublisher{
		clientset: cs,
		nodeName:  cfg.NodeName,
		baseURL:   baseURL,
		endpoints: []inventoryEndpoint{
			{annotation: storageRdmaHcasAnnotation, url: rdmaURL},
			{annotation: storageBlockDevicesAnnotation, url: blockURL},
		},
		interval: deviceInventoryPollInterval,
		httpClient: &http.Client{
			Timeout: deviceInventoryHTTPTimeout,
		},
		signal: signal,
	}

	go p.run(ctx)
}

func (p *deviceInventoryPublisher) run(ctx context.Context) {
	p.poll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *deviceInventoryPublisher) poll(ctx context.Context) {
	changed, err := p.publishOnce(ctx)
	if err != nil {
		slog.Warn("device inventory publish failed", "node", p.nodeName, "url", p.baseURL, "error", err)

		return
	}

	if changed && p.signal != nil {
		p.signal()
	}
}

func (p *deviceInventoryPublisher) publishOnce(ctx context.Context) (bool, error) {
	changed := false

	for _, endpoint := range p.endpoints {
		value, err := p.fetchAnnotationValue(ctx, endpoint.url)
		if err != nil {
			return false, fmt.Errorf("fetch %s inventory: %w", endpoint.annotation, err)
		}

		patched, err := patchNodeAnnotationIfChanged(ctx, p.clientset, p.nodeName, endpoint.annotation, value)
		if err != nil {
			return false, err
		}

		changed = changed || patched
	}

	return changed, nil
}

func (p *deviceInventoryPublisher) fetchAnnotationValue(ctx context.Context, inventoryURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inventoryURL, nil)
	if err != nil {
		return "", fmt.Errorf("build inventory request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch inventory: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch inventory: status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", fmt.Errorf("read inventory response: %w", err)
	}

	return normalizeInventoryAnnotationValue(body)
}

func patchNodeAnnotationIfChanged(
	ctx context.Context,
	cs kubernetes.Interface,
	nodeName string,
	key string,
	value string,
) (bool, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get node %q: %w", nodeName, err)
	}

	if node.Annotations[key] == value {
		return false, nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{key: value},
		},
	})
	if err != nil {
		return false, fmt.Errorf("build node annotation patch: %w", err)
	}

	if _, err := cs.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("patch node %q annotation %q: %w", nodeName, key, err)
	}

	return true, nil
}

func normalizeInventoryAnnotationValue(raw []byte) (string, error) {
	value := strings.TrimSpace(string(raw))

	entries, err := parseAnnotationList(value)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		for key, values := range entry.Values {
			if strings.TrimSpace(key) == "" {
				return "", fmt.Errorf("empty query key for %q", entry.Item)
			}

			for _, value := range values {
				if value == "" {
					return "", fmt.Errorf("empty query value for %q key %q", entry.Item, key)
				}
			}
		}
	}

	return value, nil
}

func firstRdmaInventoryAddr(value string) (string, error) {
	addrs, err := rdmaInventoryAddrs(value)
	if err != nil || len(addrs) == 0 {
		return "", err
	}

	return addrs[0], nil
}

func rdmaInventoryAddrs(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}

	entries, err := parseAnnotationList(value)
	if err != nil {
		return nil, err
	}

	var addrs []string

	for _, hca := range entries {
		for _, addr := range hca.Values["addr"] {
			if addr != "" {
				addrs = append(addrs, addr)
			}
		}
	}

	return addrs, nil
}

func inventoryURLForPath(baseURL, path string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String(), nil
}
