// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type pullIssues struct {
	Captured      bool              `json:"captured"`
	WarningEvents uint64            `json:"warning_events"`
	ByReason      map[string]uint64 `json:"by_reason"`
	Markers       map[string]uint64 `json:"markers"`
}

func (b *benchmark) fetchPullIssues(ctx context.Context, jobName string) (pullIssues, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"get", "events", "-o", "json",
	)
	if err != nil {
		return pullIssues{}, fmt.Errorf("list pull events: %w", err)
	}

	return parsePullIssues(output, jobName)
}

func parsePullIssues(raw []byte, jobName string) (pullIssues, error) {
	var events struct {
		Items []struct {
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Type    string `json:"type"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Count   int64  `json:"count"`
			Series  *struct {
				Count int64 `json:"count"`
			} `json:"series"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		return pullIssues{}, fmt.Errorf("decode pull events: %w", err)
	}

	result := pullIssues{
		Captured: true,
		ByReason: make(map[string]uint64),
		Markers:  make(map[string]uint64),
	}
	podPrefix := jobName + "-"

	for _, event := range events.Items {
		if event.Type != "Warning" || event.InvolvedObject.Kind != "Pod" || !strings.HasPrefix(event.InvolvedObject.Name, podPrefix) {
			continue
		}

		count := event.Count
		if event.Series != nil && event.Series.Count > count {
			count = event.Series.Count
		}

		if count <= 0 {
			count = 1
		}

		observations := uint64(count)
		result.WarningEvents += observations
		result.ByReason[event.Reason] += observations

		for _, marker := range classifyPullIssue(event.Message) {
			result.Markers[marker] += observations
		}
	}

	return result, nil
}

func classifyPullIssue(message string) []string {
	lower := strings.ToLower(message)
	markers := make([]string, 0, 4)

	add := func(marker string, matches ...string) {
		for _, match := range matches {
			if strings.Contains(lower, match) {
				markers = append(markers, marker)

				return
			}
		}
	}

	add("http_429", "429 too many requests", "429 toomanyrequests", "status code 429")
	add("http_5xx", "500 ", "502 ", "503 ", "504 ", "status code 500", "status code 502", "status code 503", "status code 504")
	add("acr_egress_limit", "egress is over the account limit")
	add("auth", "401 unauthorized", "403 forbidden", "failed to authorize")
	add("timeout", "timeout", "timed out", "deadline exceeded")
	add("connection_refused", "connection refused")
	add("connection_reset", "connection reset")
	add("dns", "no such host", "temporary failure in name resolution")
	add("image_pull_backoff", "imagepullbackoff", "back-off pulling image")
	add("err_image_pull", "errimagepull")

	if len(markers) == 0 {
		markers = append(markers, "other")
	}

	return markers
}
