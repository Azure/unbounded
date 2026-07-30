// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

const logAnalyticsResource = "https://api.loganalytics.io"

type telemetryWindow struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type acrPhaseMeasurement struct {
	PullCount                 uint64    `json:"pull_count"`
	SuccessfulPullCount       uint64    `json:"successful_pull_count"`
	OtherRepositoryEventCount uint64    `json:"other_repository_event_count"`
	FirstEventAt              time.Time `json:"first_event_at,omitempty"`
	LastEventAt               time.Time `json:"last_event_at,omitempty"`
	Source                    string    `json:"source"`
	Complete                  bool      `json:"complete"`
}

type privateEndpointPhaseMeasurement struct {
	BytesFromACR uint64 `json:"bytes_from_acr"`
	Source       string `json:"source"`
	Complete     bool   `json:"complete"`
}

type auditPodLatency struct {
	PodName                 string    `json:"pod_name"`
	NodeName                string    `json:"node_name"`
	CreatedAt               time.Time `json:"created_at"`
	BoundAt                 time.Time `json:"bound_at"`
	StartedStatusObservedAt time.Time `json:"started_status_observed_at"`
	StartupSeconds          float64   `json:"startup_seconds"`
	SchedulingSeconds       float64   `json:"scheduling_seconds"`
	PostBindStartupSeconds  float64   `json:"post_bind_startup_seconds"`
}

type auditPhaseMeasurement struct {
	Pods                   []auditPodLatency `json:"pods"`
	PodStartupLatency      latencySummary    `json:"pod_startup_latency"`
	SchedulingLatency      latencySummary    `json:"scheduling_latency"`
	PostBindStartupLatency latencySummary    `json:"post_bind_startup_latency"`
	Source                 string            `json:"source"`
	Complete               bool              `json:"complete"`
}

type azurePhaseMeasurement struct {
	Window          telemetryWindow                 `json:"window"`
	ACR             acrPhaseMeasurement             `json:"acr"`
	PrivateEndpoint privateEndpointPhaseMeasurement `json:"private_endpoint"`
	Audit           auditPhaseMeasurement           `json:"audit"`
	Complete        bool                            `json:"complete"`
}

type logAnalyticsResponse struct {
	Tables []struct {
		Name    string `json:"name"`
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"tables"`
}

func (b *benchmark) queryLogAnalytics(
	ctx context.Context,
	basic bool,
	query string,
	window telemetryWindow,
) ([]map[string]any, error) {
	endpoint := "query"
	if basic {
		endpoint = "search"
	}

	rows, err := b.queryLogAnalyticsEndpoint(ctx, endpoint, query, window)
	if err == nil || !basic {
		return rows, err
	}

	// AKSAuditAdmin supports both Basic and Analytics plans. /search is
	// required for Basic; /query is required for Analytics.
	rows, fallbackErr := b.queryLogAnalyticsEndpoint(ctx, "query", query, window)
	if fallbackErr != nil {
		return nil, fmt.Errorf("query Log Analytics with /search (%v) and /query fallback: %w", err, fallbackErr)
	}

	return rows, nil
}

func (b *benchmark) queryLogAnalyticsEndpoint(
	ctx context.Context,
	endpoint string,
	query string,
	window telemetryWindow,
) ([]map[string]any, error) {
	timespan := window.StartedAt.UTC().Format(time.RFC3339Nano) + "/" + window.FinishedAt.UTC().Format(time.RFC3339Nano)
	requestURL := fmt.Sprintf(
		"%s/v1/workspaces/%s/%s?timespan=%s",
		logAnalyticsResource,
		url.PathEscape(b.config.LogAnalyticsWorkspaceID),
		endpoint,
		url.QueryEscape(timespan),
	)

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("encode Log Analytics query: %w", err)
	}

	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "rest",
		"--method", "post",
		"--url", requestURL,
		"--resource", logAnalyticsResource,
		"--headers", "Content-Type=application/json",
		"--body", string(body),
		"--output", "json",
	)
	if err != nil {
		return nil, fmt.Errorf("query Log Analytics: %w", err)
	}

	return decodeLogAnalyticsRows(output)
}

func decodeLogAnalyticsRows(raw []byte) ([]map[string]any, error) {
	var response logAnalyticsResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode Log Analytics response: %w", err)
	}

	if len(response.Tables) != 1 {
		return nil, fmt.Errorf("log Analytics response has %d tables, want 1", len(response.Tables))
	}

	table := response.Tables[0]
	rows := make([]map[string]any, 0, len(table.Rows))

	for rowIndex, values := range table.Rows {
		if len(values) != len(table.Columns) {
			return nil, fmt.Errorf(
				"log Analytics row %d has %d values, want %d",
				rowIndex,
				len(values),
				len(table.Columns),
			)
		}

		row := make(map[string]any, len(values))
		for index, column := range table.Columns {
			row[column.Name] = values[index]
		}

		rows = append(rows, row)
	}

	return rows, nil
}

func kustoQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func imageDigestFromReference(image string) (string, error) {
	_, digest, ok := strings.Cut(image, "@")
	if !ok || digest == "" {
		return "", fmt.Errorf("image reference %q has no digest", image)
	}

	return digest, nil
}

func (b *benchmark) collectACRPulls(
	ctx context.Context,
	image string,
	window telemetryWindow,
) (acrPhaseMeasurement, error) {
	digest, err := imageDigestFromReference(image)
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	query := fmt.Sprintf(`ContainerRegistryRepositoryEvents
| where _ResourceId =~ %s
| extend IsPhasePull = Repository == %s and Digest == %s and OperationName == "Pull"
| summarize pull_count=countif(IsPhasePull), successful_pull_count=countif(IsPhasePull and ResultDescription == "200"), other_repository_event_count=countif(not(IsPhasePull)), first_event_at=minif(TimeGenerated, IsPhasePull), last_event_at=maxif(TimeGenerated, IsPhasePull)`,
		kustoQuote(b.config.ACRResourceID),
		kustoQuote(b.config.WorkloadRepository),
		kustoQuote(digest),
	)

	rows, err := b.queryLogAnalytics(ctx, false, query, window)
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	if len(rows) != 1 {
		return acrPhaseMeasurement{}, fmt.Errorf("ACR pull query returned %d rows, want 1", len(rows))
	}

	pullCount, err := rowUint64(rows[0], "pull_count")
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	successfulPullCount, err := rowUint64(rows[0], "successful_pull_count")
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	otherRepositoryEventCount, err := rowUint64(rows[0], "other_repository_event_count")
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	firstEventAt, err := rowOptionalTime(rows[0], "first_event_at")
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	lastEventAt, err := rowOptionalTime(rows[0], "last_event_at")
	if err != nil {
		return acrPhaseMeasurement{}, err
	}

	return acrPhaseMeasurement{
		PullCount:                 pullCount,
		SuccessfulPullCount:       successfulPullCount,
		OtherRepositoryEventCount: otherRepositoryEventCount,
		FirstEventAt:              firstEventAt,
		LastEventAt:               lastEventAt,
		Source:                    "ContainerRegistryRepositoryEvents",
		Complete:                  pullCount > 0 && pullCount == successfulPullCount && otherRepositoryEventCount == 0,
	}, nil
}

type azureMetricsResponse struct {
	Value []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
		Timeseries []struct {
			Data []struct {
				Total *float64 `json:"total"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"value"`
}

func (b *benchmark) collectPrivateEndpointBytes(
	ctx context.Context,
	window telemetryWindow,
) (privateEndpointPhaseMeasurement, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "monitor", "metrics", "list",
		"--resource", b.config.ACRPrivateEndpointResourceID,
		"--metric", "PEBytesIn",
		"--interval", "PT1M",
		"--start-time", window.StartedAt.UTC().Format(time.RFC3339Nano),
		"--end-time", window.FinishedAt.UTC().Format(time.RFC3339Nano),
		"--aggregation", "Total",
		"--output", "json",
	)
	if err != nil {
		return privateEndpointPhaseMeasurement{}, fmt.Errorf("query private endpoint bytes: %w", err)
	}

	var response azureMetricsResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return privateEndpointPhaseMeasurement{}, fmt.Errorf("decode private endpoint metrics: %w", err)
	}

	var (
		total      float64
		dataPoints int
	)

	for _, metric := range response.Value {
		if metric.Name.Value != "PEBytesIn" {
			continue
		}

		for _, series := range metric.Timeseries {
			for _, point := range series.Data {
				if point.Total == nil {
					continue
				}

				total += *point.Total
				dataPoints++
			}
		}
	}

	if total < 0 || total > math.MaxUint64 || math.Trunc(total) != total {
		return privateEndpointPhaseMeasurement{}, fmt.Errorf("private endpoint PEBytesIn total %v is not a uint64", total)
	}

	return privateEndpointPhaseMeasurement{
		BytesFromACR: uint64(total),
		Source:       "Microsoft.Network/privateEndpoints/PEBytesIn",
		Complete:     dataPoints > 0,
	}, nil
}

type auditAccumulator struct {
	createdAt time.Time
	boundAt   time.Time
	startedAt time.Time
}

func (b *benchmark) collectAuditLatency(
	ctx context.Context,
	job jobObservation,
	window telemetryWindow,
) (auditPhaseMeasurement, error) {
	query := fmt.Sprintf(`AKSAuditAdmin
| where _ResourceId =~ %s
| extend ParsedObjectRef=parse_json(ObjectRef), ParsedRequestObject=parse_json(RequestObject), ParsedResponseObject=parse_json(ResponseObject)
| extend Resource=tostring(ParsedObjectRef.resource), Subresource=tostring(ParsedObjectRef.subresource), Namespace=tostring(ParsedObjectRef.namespace), ObjectName=tostring(ParsedObjectRef.name), ResponseName=tostring(ParsedResponseObject.metadata.name), GenerateName=tostring(ParsedRequestObject.metadata.generateName)
| extend Name=iff(isempty(ObjectName), ResponseName, ObjectName)
| where Namespace == %s and (Name startswith %s or GenerateName startswith %s)
| where Resource == "pods" and (Verb == "create" or Verb == "patch" or Verb == "update")
| project RequestReceivedTime, Verb, Subresource, Name, RequestObject`,
		kustoQuote(b.config.AKSResourceID),
		kustoQuote(b.config.Namespace),
		kustoQuote(job.JobName+"-"),
		kustoQuote(job.JobName+"-"),
	)

	searchWindow := telemetryWindow{
		StartedAt:  window.StartedAt.Add(-5 * time.Minute),
		FinishedAt: window.FinishedAt.Add(5 * time.Minute),
	}

	rows, err := b.queryLogAnalytics(ctx, true, query, searchWindow)
	if err != nil {
		return auditPhaseMeasurement{}, err
	}

	expected := make(map[string]struct{}, len(job.Pods))
	for _, pod := range job.Pods {
		expected[pod] = struct{}{}
	}

	accumulators := make(map[string]*auditAccumulator, len(expected))

	for _, row := range rows {
		podName, err := rowString(row, "Name")
		if err != nil {
			return auditPhaseMeasurement{}, err
		}

		if _, ok := expected[podName]; !ok {
			continue
		}

		receivedAt, err := rowTime(row, "RequestReceivedTime")
		if err != nil {
			return auditPhaseMeasurement{}, err
		}

		verb, err := rowString(row, "Verb")
		if err != nil {
			return auditPhaseMeasurement{}, err
		}

		subresource, err := rowString(row, "Subresource")
		if err != nil {
			return auditPhaseMeasurement{}, err
		}

		acc := accumulators[podName]
		if acc == nil {
			acc = &auditAccumulator{}
			accumulators[podName] = acc
		}

		switch {
		case verb == "create" && subresource == "":
			acc.createdAt = earlierNonZero(acc.createdAt, receivedAt)
		case verb == "create" && subresource == "binding":
			acc.boundAt = earlierNonZero(acc.boundAt, receivedAt)
		case (verb == "patch" || verb == "update") && subresource == "status":
			started, err := auditRequestHasStartedContainer(row["RequestObject"])
			if err != nil {
				return auditPhaseMeasurement{}, fmt.Errorf("parse pod %s status request: %w", podName, err)
			}

			if started {
				acc.startedAt = earlierNonZero(acc.startedAt, receivedAt)
			}
		}
	}

	pods := make([]auditPodLatency, 0, len(expected))
	startup := make([]float64, 0, len(expected))
	scheduling := make([]float64, 0, len(expected))
	postBind := make([]float64, 0, len(expected))

	for _, podName := range sortedMapKeys(expected) {
		acc := accumulators[podName]
		if acc == nil || acc.createdAt.IsZero() || acc.boundAt.IsZero() || acc.startedAt.IsZero() {
			continue
		}

		if acc.boundAt.Before(acc.createdAt) || acc.startedAt.Before(acc.boundAt) {
			return auditPhaseMeasurement{}, fmt.Errorf("pod %s has non-monotonic audit timestamps", podName)
		}

		startupSeconds := acc.startedAt.Sub(acc.createdAt).Seconds()
		schedulingSeconds := acc.boundAt.Sub(acc.createdAt).Seconds()
		postBindSeconds := acc.startedAt.Sub(acc.boundAt).Seconds()

		pods = append(pods, auditPodLatency{
			PodName:                 podName,
			NodeName:                job.PodNodes[podName],
			CreatedAt:               acc.createdAt,
			BoundAt:                 acc.boundAt,
			StartedStatusObservedAt: acc.startedAt,
			StartupSeconds:          startupSeconds,
			SchedulingSeconds:       schedulingSeconds,
			PostBindStartupSeconds:  postBindSeconds,
		})
		startup = append(startup, startupSeconds)
		scheduling = append(scheduling, schedulingSeconds)
		postBind = append(postBind, postBindSeconds)
	}

	return auditPhaseMeasurement{
		Pods:                   pods,
		PodStartupLatency:      summarizeFloatSeconds(startup),
		SchedulingLatency:      summarizeFloatSeconds(scheduling),
		PostBindStartupLatency: summarizeFloatSeconds(postBind),
		Source:                 "AKSAuditAdmin",
		Complete:               len(pods) == len(expected) && len(expected) > 0,
	}, nil
}

func rowString(row map[string]any, name string) (string, error) {
	value, ok := row[name]
	if !ok || value == nil {
		return "", nil
	}

	result, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("log Analytics column %q has type %T, want string", name, value)
	}

	return result, nil
}

func rowTime(row map[string]any, name string) (time.Time, error) {
	value, err := rowString(row, name)
	if err != nil {
		return time.Time{}, err
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Log Analytics column %q time %q: %w", name, value, err)
	}

	return parsed, nil
}

func earlierNonZero(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}

	return current
}

func auditRequestHasStartedContainer(value any) (bool, error) {
	if value == nil {
		return false, nil
	}

	var raw []byte

	switch typed := value.(type) {
	case string:
		if typed == "" || typed == "skipped-too-big-size-object" {
			return false, nil
		}

		raw = []byte(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return false, err
		}

		raw = encoded
	}

	var object struct {
		Status struct {
			ContainerStatuses []struct {
				State struct {
					Running *struct {
						StartedAt string `json:"startedAt"`
					} `json:"running"`
					Terminated *struct {
						StartedAt string `json:"startedAt"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}

	if err := json.Unmarshal(raw, &object); err != nil {
		return false, err
	}

	for _, status := range object.Status.ContainerStatuses {
		if (status.State.Running != nil && status.State.Running.StartedAt != "") ||
			(status.State.Terminated != nil && status.State.Terminated.StartedAt != "") {
			return true, nil
		}
	}

	return false, nil
}

func rowUint64(row map[string]any, name string) (uint64, error) {
	value, ok := row[name]
	if !ok {
		return 0, fmt.Errorf("log Analytics row has no %q column", name)
	}

	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed > math.MaxUint64 || math.Trunc(typed) != typed {
			return 0, fmt.Errorf("log Analytics column %q value %v is not a uint64", name, typed)
		}

		return uint64(typed), nil
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("parse Log Analytics column %q value %q", name, typed)
		}

		return uint64(parsed), nil
	default:
		return 0, fmt.Errorf("log Analytics column %q has type %T, want number", name, value)
	}
}

func rowOptionalTime(row map[string]any, name string) (time.Time, error) {
	value, ok := row[name]
	if !ok || value == nil {
		return time.Time{}, nil
	}

	raw, ok := value.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("log Analytics column %q has type %T, want string", name, value)
	}

	if raw == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Log Analytics column %q time %q: %w", name, raw, err)
	}

	return parsed, nil
}

func summarizeFloatSeconds(values []float64) latencySummary {
	durations := make([]time.Duration, 0, len(values))
	for _, value := range values {
		durations = append(durations, time.Duration(value*float64(time.Second)))
	}

	return summarizeLatencies(durations)
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
