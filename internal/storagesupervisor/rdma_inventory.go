// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	storageRdmaHcasAnnotation = "storage.unbounded-cloud.io/rdma-hcas"
	rdmaInventoryPollInterval = 30 * time.Second
	rdmaInventoryHTTPTimeout  = 5 * time.Second
)

type rdmaInventory struct {
	SchemaVersion int       `json:"schemaVersion"`
	HCAs          []rdmaHCA `json:"hcas"`
}

type rdmaHCA struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"`
}

type rdmaInventoryPublisher struct {
	clientset  kubernetes.Interface
	nodeName   string
	url        string
	interval   time.Duration
	httpClient *http.Client
	signal     func()
}

func startRdmaInventoryPublisher(ctx context.Context, cfg Config, cs kubernetes.Interface, signal func()) {
	if cfg.RdmaInventoryURL == "" {
		return
	}

	if cfg.NodeName == "" {
		slog.Warn("rdma inventory publishing disabled: NODE_NAME is not set", "url", cfg.RdmaInventoryURL)

		return
	}

	p := &rdmaInventoryPublisher{
		clientset: cs,
		nodeName:  cfg.NodeName,
		url:       cfg.RdmaInventoryURL,
		interval:  rdmaInventoryPollInterval,
		httpClient: &http.Client{
			Timeout: rdmaInventoryHTTPTimeout,
		},
		signal: signal,
	}

	go p.run(ctx)
}

func (p *rdmaInventoryPublisher) run(ctx context.Context) {
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

func (p *rdmaInventoryPublisher) poll(ctx context.Context) {
	changed, err := p.publishOnce(ctx)
	if err != nil {
		slog.Warn("rdma inventory publish failed", "node", p.nodeName, "url", p.url, "error", err)

		return
	}

	if changed && p.signal != nil {
		p.signal()
	}
}

func (p *rdmaInventoryPublisher) publishOnce(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return false, fmt.Errorf("build inventory request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("fetch inventory: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("fetch inventory: status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return false, fmt.Errorf("read inventory response: %w", err)
	}

	jsonValue, _, err := normalizeRdmaInventoryJSON(body)
	if err != nil {
		return false, fmt.Errorf("validate inventory response: %w", err)
	}

	return patchNodeAnnotationIfChanged(ctx, p.clientset, p.nodeName, storageRdmaHcasAnnotation, jsonValue)
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

func normalizeRdmaInventoryJSON(raw []byte) (string, *rdmaInventory, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, bytes.TrimSpace(raw)); err != nil {
		return "", nil, err
	}

	var inv rdmaInventory
	if err := json.Unmarshal(compact.Bytes(), &inv); err != nil {
		return "", nil, err
	}

	if inv.SchemaVersion != 1 {
		return "", nil, fmt.Errorf("unsupported schemaVersion %d", inv.SchemaVersion)
	}

	if inv.HCAs == nil {
		return "", nil, fmt.Errorf("hcas field is required")
	}

	return compact.String(), &inv, nil
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

	_, inv, err := normalizeRdmaInventoryJSON([]byte(value))
	if err != nil {
		return nil, err
	}

	var addrs []string

	for _, hca := range inv.HCAs {
		for _, addr := range hca.Addrs {
			if addr != "" {
				addrs = append(addrs, addr)
			}
		}
	}

	return addrs, nil
}
