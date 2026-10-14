// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var errWorkflowNodesOffloaded = errors.New("WorkflowNodesOffloaded")

func workflowNodes(workflow *unstructured.Unstructured) (map[string]any, error) {
	if workflow == nil {
		return nil, nil
	}

	status, found, err := unstructured.NestedMap(workflow.Object, "status")
	if err != nil {
		return nil, fmt.Errorf("WorkflowNodesMalformed: %w", err)
	}

	if !found {
		return nil, nil
	}

	if raw, present := status["offloadNodeStatusVersion"]; present {
		version, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("WorkflowNodesMalformed: offloadNodeStatusVersion must be a string")
		}

		if version != "" {
			return nil, errWorkflowNodesOffloaded
		}
	}

	if raw, present := status["compressedNodes"]; present {
		encoded, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: compressedNodes must be a string")
		}

		if encoded != "" {
			return decompressWorkflowNodes(encoded)
		}
	}

	nodes, _, err := unstructured.NestedMap(status, "nodes")
	if err != nil {
		return nil, fmt.Errorf("WorkflowNodesMalformed: %w", err)
	}

	return validateWorkflowNodeMap(nodes)
}

func decompressWorkflowNodes(encoded string) (map[string]any, error) {
	reader, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
	if err != nil {
		return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: %w", err)
	}

	data, readErr := io.ReadAll(io.LimitReader(reader, maxEvidenceBytes+1))
	closeErr := reader.Close()

	if readErr != nil {
		return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: %w", readErr)
	}

	if len(data) > maxEvidenceBytes {
		return nil, fmt.Errorf("WorkflowNodesTooLarge: decompressed nodes exceed %d bytes", maxEvidenceBytes)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: %w", closeErr)
	}

	var nodes map[string]any
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: %w", err)
	}

	if nodes == nil {
		return nil, fmt.Errorf("WorkflowNodesCompressedMalformed: nodes must be a JSON object")
	}

	return validateWorkflowNodeMap(nodes)
}

func validateWorkflowNodeMap(nodes map[string]any) (map[string]any, error) {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		raw := nodes[id]
		if node, ok := raw.(map[string]any); !ok || node == nil || id == "" {
			return nil, fmt.Errorf("WorkflowNodesMalformed: node %q must be an object with a nonempty ID", id)
		}
	}

	return nodes, nil
}

func workflowNodeStatus(workflow *unstructured.Unstructured) map[string]any {
	if workflow == nil {
		return nil
	}

	status, found, err := unstructured.NestedMap(workflow.Object, "status")
	if err != nil || !found {
		return nil
	}

	markers := map[string]any{}

	for _, name := range []string{"compressedNodes", "offloadNodeStatusVersion"} {
		if raw, present := status[name]; present {
			markers[name] = raw
		}
	}

	if len(markers) != 0 {
		if nodes, present := status["nodes"]; present {
			markers["nodes"] = nodes
		}
	}

	return markers
}
