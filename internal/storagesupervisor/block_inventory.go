// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	storageBlockDevicesAnnotation = "storage.unbounded-cloud.io/block-devices"
	blockInventoryPollInterval    = 30 * time.Second
	blockInventoryHTTPTimeout     = 5 * time.Second
)

type blockInventoryPublisher struct {
	clientset  kubernetes.Interface
	nodeName   string
	url        string
	interval   time.Duration
	httpClient *http.Client
	signal     func()
}

func startBlockInventoryPublisher(ctx context.Context, cfg Config, cs kubernetes.Interface, signal func()) {
	if cfg.BlockInventoryURL == "" {
		return
	}

	if cfg.NodeName == "" {
		slog.Warn("block inventory publishing disabled: NODE_NAME is not set", "url", cfg.BlockInventoryURL)

		return
	}

	p := &blockInventoryPublisher{
		clientset: cs,
		nodeName:  cfg.NodeName,
		url:       cfg.BlockInventoryURL,
		interval:  blockInventoryPollInterval,
		httpClient: &http.Client{
			Timeout: blockInventoryHTTPTimeout,
		},
		signal: signal,
	}

	go p.run(ctx)
}

func (p *blockInventoryPublisher) run(ctx context.Context) {
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

func (p *blockInventoryPublisher) poll(ctx context.Context) {
	changed, err := p.publishOnce(ctx)
	if err != nil {
		slog.Warn("block inventory publish failed", "node", p.nodeName, "url", p.url, "error", err)

		return
	}

	if changed && p.signal != nil {
		p.signal()
	}
}

func (p *blockInventoryPublisher) publishOnce(ctx context.Context) (bool, error) {
	value, err := p.fetchAnnotationValue(ctx)
	if err != nil {
		return false, fmt.Errorf("fetch inventory: %w", err)
	}

	return patchNodeAnnotationIfChanged(ctx, p.clientset, p.nodeName, storageBlockDevicesAnnotation, value)
}

func (p *blockInventoryPublisher) fetchAnnotationValue(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
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
