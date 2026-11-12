// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	metricTelemetryCurrentName = "project_signal_certification_metric"
	metricTelemetrySampleName  = "project_signal_certification_metric_sample"
	metricTelemetryMaxBytes    = 64 * 1024
	metricTelemetryMaxLine     = 2048
)

var metricTelemetryReservedDimensions = map[string]struct{}{
	"invocation":       {},
	"metric_id":        {},
	"namespace":        {},
	"node":             {},
	"participant_role": {},
	"run":              {},
	"run_uid":          {},
	"sample":           {},
	"stage":            {},
	"unit":             {},
}

type MetricProxyIdentity struct {
	Namespace       string
	Run             string
	RunUID          string
	Stage           string
	Invocation      string
	ParticipantRole string
	Node            string
}

type MetricProxyConfig struct {
	ContractJSON string
	RecordsPath  string
	DonePath     string
	Port         int
	FinalGrace   time.Duration
	Identity     MetricProxyIdentity
}

type metricRecord struct {
	Metric     string            `json:"metric"`
	Value      json.Number       `json:"value"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
}

type metricSeries struct {
	definition signalv1.TestMetricDefinition
	dimensions map[string]string
	samples    []metricSample
}

type metricSample struct {
	value    float64
	sequence int
}

type metricTelemetryResult struct {
	Prometheus string
	Final      map[string]float64
}

func metricTelemetryContract(
	definitions []signalv1.TestMetricDefinition,
) (string, bool, error) {
	declared := make([]signalv1.TestMetricDefinition, 0, len(definitions))
	for index := range definitions {
		if definitions[index].Telemetry != nil {
			declared = append(declared, *definitions[index].DeepCopy())
		}
	}

	if len(declared) == 0 {
		return "", false, nil
	}

	content, err := json.Marshal(declared)
	if err != nil {
		return "", false, fmt.Errorf("encode metric telemetry contract: %w", err)
	}

	if _, err := decodeMetricTelemetryContract(string(content)); err != nil {
		return "", false, err
	}

	return string(content), true, nil
}

func decodeMetricTelemetryContract(content string) (map[string]signalv1.TestMetricDefinition, error) {
	var definitions []signalv1.TestMetricDefinition

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&definitions); err != nil {
		return nil, fmt.Errorf("decode metric telemetry contract: %w", err)
	}

	if len(definitions) == 0 || len(definitions) > signalv1.MaxTestMetrics {
		return nil, errors.New("metric telemetry contract has an invalid metric count")
	}

	result := make(map[string]signalv1.TestMetricDefinition, len(definitions))
	for index := range definitions {
		definition := definitions[index]

		telemetry := definition.Telemetry
		if telemetry == nil {
			return nil, fmt.Errorf("metric %q does not declare telemetry", definition.Name)
		}

		if _, exists := result[definition.Name]; exists {
			return nil, fmt.Errorf("metric %q is declared more than once", definition.Name)
		}

		if !metricSupportsTelemetry(definition.Type) || definition.Unit == "" {
			return nil, fmt.Errorf("metric %q is not a unit-bearing decimal metric", definition.Name)
		}

		if telemetry.Live != signalv1.MetricLiveModeGauge &&
			telemetry.Live != signalv1.MetricLiveModeGaugeAndSamples {
			return nil, fmt.Errorf("metric %q has unsupported live mode %q", definition.Name, telemetry.Live)
		}

		if telemetry.SeriesLimit < 1 || telemetry.SeriesLimit > signalv1.MaxMetricSeries ||
			telemetry.SampleLimit < 1 || telemetry.SampleLimit > signalv1.MaxMetricSamples {
			return nil, fmt.Errorf("metric %q has invalid telemetry limits", definition.Name)
		}

		if len(telemetry.Dimensions) > signalv1.MaxMetricDimensions {
			return nil, fmt.Errorf("metric %q has too many dimensions", definition.Name)
		}

		seenDimensions := map[string]struct{}{}

		for _, rawName := range telemetry.Dimensions {
			name := string(rawName)
			if _, reserved := metricTelemetryReservedDimensions[name]; reserved {
				return nil, fmt.Errorf("metric %q declares reserved dimension %q", definition.Name, name)
			}

			if _, exists := seenDimensions[name]; exists {
				return nil, fmt.Errorf("metric %q repeats dimension %q", definition.Name, name)
			}

			seenDimensions[name] = struct{}{}
		}

		switch telemetry.Evidence {
		case "", signalv1.MetricEvidenceLatest, signalv1.MetricEvidenceMinimum,
			signalv1.MetricEvidenceMaximum:
		default:
			return nil, fmt.Errorf(
				"metric %q has unsupported evidence aggregation %q",
				definition.Name,
				telemetry.Evidence,
			)
		}

		result[definition.Name] = definition
	}

	return result, nil
}

func metricSupportsTelemetry(metricType signalv1.MetricValueType) bool {
	switch metricType {
	case signalv1.MetricValueTypeGauge,
		signalv1.MetricValueTypeRatio,
		signalv1.MetricValueTypeThroughput,
		signalv1.MetricValueTypeIOPS,
		signalv1.MetricValueTypeBandwidth,
		signalv1.MetricValueTypeLatency:
		return true
	default:
		return false
	}
}

func processMetricTelemetry(
	contractJSON string,
	content []byte,
	complete bool,
	identity MetricProxyIdentity,
) (metricTelemetryResult, error) {
	definitions, err := decodeMetricTelemetryContract(contractJSON)
	if err != nil {
		return metricTelemetryResult{}, err
	}

	if len(content) > metricTelemetryMaxBytes {
		return metricTelemetryResult{}, fmt.Errorf(
			"metric record stream exceeds %d bytes",
			metricTelemetryMaxBytes,
		)
	}

	if !complete {
		lastNewline := bytes.LastIndexByte(content, '\n')
		if lastNewline < 0 {
			content = nil
		} else {
			content = content[:lastNewline+1]
		}
	}

	series := map[string]*metricSeries{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, metricTelemetryMaxLine), metricTelemetryMaxLine)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++

		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		record, err := decodeMetricRecord(line)
		if err != nil {
			return metricTelemetryResult{}, fmt.Errorf("metric record %d: %w", lineNumber, err)
		}

		definition, exists := definitions[record.Metric]
		if !exists {
			return metricTelemetryResult{}, fmt.Errorf("metric record %d declares unknown metric %q", lineNumber, record.Metric)
		}

		if err := validateMetricDimensions(definition, record.Dimensions); err != nil {
			return metricTelemetryResult{}, fmt.Errorf("metric record %d: %w", lineNumber, err)
		}

		value, err := strconv.ParseFloat(record.Value.String(), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return metricTelemetryResult{}, fmt.Errorf("metric record %d has a non-finite value", lineNumber)
		}

		key := metricSeriesKey(record.Metric, record.Dimensions)

		current, exists := series[key]
		if !exists {
			if metricSeriesCount(series, record.Metric) >= int(definition.Telemetry.SeriesLimit) {
				return metricTelemetryResult{}, fmt.Errorf(
					"metric %q exceeds series limit %d",
					record.Metric,
					definition.Telemetry.SeriesLimit,
				)
			}

			current = &metricSeries{
				definition: definition,
				dimensions: record.Dimensions,
			}
			series[key] = current
		}

		if len(current.samples) >= int(definition.Telemetry.SampleLimit) {
			return metricTelemetryResult{}, fmt.Errorf(
				"metric %q exceeds sample limit %d",
				record.Metric,
				definition.Telemetry.SampleLimit,
			)
		}

		current.samples = append(current.samples, metricSample{
			value:    value,
			sequence: lineNumber,
		})
	}

	if err := scanner.Err(); err != nil {
		return metricTelemetryResult{}, fmt.Errorf("scan metric records: %w", err)
	}

	if complete && len(content) > 0 && content[len(content)-1] != '\n' {
		return metricTelemetryResult{}, errors.New("metric record stream ends with an incomplete record")
	}

	if complete {
		for name, definition := range definitions {
			if definition.Required && definition.Telemetry.Evidence != "" &&
				metricSeriesCount(series, name) == 0 {
				return metricTelemetryResult{}, fmt.Errorf("required metric %q has no records", name)
			}
		}
	}

	return metricTelemetryResult{
		Prometheus: renderMetricPrometheus(series, identity),
		Final:      aggregateFinalMetricEvidence(series),
	}, nil
}

func decodeMetricRecord(content []byte) (metricRecord, error) {
	var record metricRecord

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&record); err != nil {
		return metricRecord{}, fmt.Errorf("decode JSON: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return metricRecord{}, errors.New("record contains trailing JSON")
	}

	if record.Metric == "" || record.Value == "" {
		return metricRecord{}, errors.New("metric and value are required")
	}

	if record.Dimensions == nil {
		record.Dimensions = map[string]string{}
	}

	return record, nil
}

func validateMetricDimensions(
	definition signalv1.TestMetricDefinition,
	dimensions map[string]string,
) error {
	allowed := make(map[string]struct{}, len(definition.Telemetry.Dimensions))
	for _, name := range definition.Telemetry.Dimensions {
		allowed[string(name)] = struct{}{}
	}

	if len(dimensions) != len(allowed) {
		return fmt.Errorf("metric %q dimensions do not match the declaration", definition.Name)
	}

	for name, value := range dimensions {
		if _, exists := allowed[name]; !exists {
			return fmt.Errorf("metric %q uses undeclared dimension %q", definition.Name, name)
		}

		if len(value) == 0 || len(value) > 63 ||
			strings.ContainsAny(value, "\n\r\x00") {
			return fmt.Errorf("metric %q dimension %q has an invalid value", definition.Name, name)
		}
	}

	return nil
}

func metricSeriesKey(metric string, dimensions map[string]string) string {
	names := make([]string, 0, len(dimensions))
	for name := range dimensions {
		names = append(names, name)
	}

	sort.Strings(names)

	var builder strings.Builder
	builder.WriteString(metric)

	for _, name := range names {
		builder.WriteByte(0)
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(dimensions[name])
	}

	return builder.String()
}

func metricSeriesCount(series map[string]*metricSeries, metric string) int {
	count := 0

	for _, current := range series {
		if current.definition.Name == metric {
			count++
		}
	}

	return count
}

func renderMetricPrometheus(
	series map[string]*metricSeries,
	identity MetricProxyIdentity,
) string {
	keys := make([]string, 0, len(series))
	for key := range series {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString("# TYPE " + metricTelemetryCurrentName + " gauge\n")

	for _, key := range keys {
		current := series[key]
		if len(current.samples) == 0 {
			continue
		}

		labels := metricTelemetryLabels(current, identity)
		writePrometheusSample(
			&builder,
			metricTelemetryCurrentName,
			labels,
			current.samples[len(current.samples)-1].value,
		)
	}

	builder.WriteString("# TYPE " + metricTelemetrySampleName + " gauge\n")

	for _, key := range keys {
		current := series[key]
		if current.definition.Telemetry.Live != signalv1.MetricLiveModeGaugeAndSamples {
			continue
		}

		for index, sample := range current.samples {
			labels := metricTelemetryLabels(current, identity)
			labels["sample"] = strconv.Itoa(index + 1)
			writePrometheusSample(&builder, metricTelemetrySampleName, labels, sample.value)
		}
	}

	return builder.String()
}

func metricTelemetryLabels(
	series *metricSeries,
	identity MetricProxyIdentity,
) map[string]string {
	labels := map[string]string{
		"invocation":       identity.Invocation,
		"metric_id":        series.definition.Name,
		"namespace":        identity.Namespace,
		"node":             identity.Node,
		"participant_role": identity.ParticipantRole,
		"run":              identity.Run,
		"run_uid":          identity.RunUID,
		"stage":            identity.Stage,
		"unit":             series.definition.Unit,
	}
	for name, value := range series.dimensions {
		labels[name] = value
	}

	return labels
}

func writePrometheusSample(
	builder *strings.Builder,
	name string,
	labels map[string]string,
	value float64,
) {
	names := make([]string, 0, len(labels))
	for label := range labels {
		names = append(names, label)
	}

	sort.Strings(names)

	builder.WriteString(name)
	builder.WriteByte('{')

	for index, label := range names {
		if index > 0 {
			builder.WriteByte(',')
		}

		builder.WriteString(label)
		builder.WriteString(`="`)
		builder.WriteString(prometheusLabelValue(labels[label]))
		builder.WriteByte('"')
	}

	builder.WriteString("} ")
	builder.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	builder.WriteByte('\n')
}

func prometheusLabelValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)

	return strings.ReplaceAll(value, `"`, `\"`)
}

func aggregateFinalMetricEvidence(series map[string]*metricSeries) map[string]float64 {
	samples := map[string][]metricSample{}
	definitions := map[string]signalv1.TestMetricDefinition{}

	for _, current := range series {
		if current.definition.Telemetry.Evidence == "" {
			continue
		}

		samples[current.definition.Name] = append(samples[current.definition.Name], current.samples...)
		definitions[current.definition.Name] = current.definition
	}

	result := make(map[string]float64, len(samples))
	for name, observed := range samples {
		if len(observed) == 0 {
			continue
		}

		value := observed[0]

		switch definitions[name].Telemetry.Evidence {
		case signalv1.MetricEvidenceLatest:
			for _, candidate := range observed[1:] {
				if candidate.sequence > value.sequence {
					value = candidate
				}
			}
		case signalv1.MetricEvidenceMinimum:
			for _, candidate := range observed[1:] {
				if candidate.value < value.value {
					value = candidate
				}
			}
		case signalv1.MetricEvidenceMaximum:
			for _, candidate := range observed[1:] {
				if candidate.value > value.value {
					value = candidate
				}
			}
		}

		result[name] = value.value
	}

	return result
}

func RunMetricProxy(ctx context.Context, config MetricProxyConfig) error {
	if config.Port < 1 || config.Port > 65535 {
		return errors.New("metric proxy port is invalid")
	}

	if config.FinalGrace < 0 {
		return errors.New("metric proxy final grace is invalid")
	}

	for name, value := range map[string]string{
		"records":          config.RecordsPath,
		"done":             config.DonePath,
		"namespace":        config.Identity.Namespace,
		"run":              config.Identity.Run,
		"run UID":          config.Identity.RunUID,
		"stage":            config.Identity.Stage,
		"invocation":       config.Identity.Invocation,
		"participant role": config.Identity.ParticipantRole,
		"node":             config.Identity.Node,
	} {
		if value == "" {
			return fmt.Errorf("metric proxy %s is required", name)
		}
	}

	if _, err := decodeMetricTelemetryContract(config.ContractJSON); err != nil {
		return err
	}

	var state struct {
		sync.RWMutex
		latest  string
		final   []byte
		failure string
	}

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.RLock()
		defer state.RUnlock()

		switch request.URL.Path {
		case "/metrics":
			writer.Header().Set("Content-Type", "text/plain; version=0.0.4")

			if _, err := writer.Write([]byte(state.latest)); err != nil {
				return
			}
		case "/final":
			if state.failure != "" {
				http.Error(writer, state.failure, http.StatusUnprocessableEntity)
				return
			}

			if state.final == nil {
				writer.WriteHeader(http.StatusAccepted)
				return
			}

			writer.Header().Set("Content-Type", "application/json")

			if _, err := writer.Write(state.final); err != nil {
				return
			}
		default:
			http.NotFound(writer, request)
		}
	})
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(config.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)

	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}

		close(serverErrors)
	}()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := server.Shutdown(context.Background()); err != nil {
				return errors.Join(ctx.Err(), err)
			}

			return ctx.Err()
		case err, open := <-serverErrors:
			if !open {
				return errors.New("metric telemetry server stopped unexpectedly")
			}

			if err != nil {
				return fmt.Errorf("serve metric telemetry: %w", err)
			}
		case <-ticker.C:
			content, err := os.ReadFile(config.RecordsPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}

			_, doneErr := os.Stat(config.DonePath)

			done := doneErr == nil
			if doneErr != nil && !errors.Is(doneErr, os.ErrNotExist) {
				return doneErr
			}

			result, err := processMetricTelemetry(config.ContractJSON, content, done, config.Identity)
			if err != nil {
				state.Lock()
				if done {
					state.failure = err.Error()
				} else {
					state.latest = ""
				}
				state.Unlock()

				if done {
					break
				}

				continue
			}

			state.Lock()
			state.latest = result.Prometheus
			state.Unlock()

			if !done {
				continue
			}

			final, err := json.Marshal(result.Final)
			if err != nil {
				state.Lock()
				state.failure = fmt.Sprintf("encode final metric telemetry: %v", err)
				state.Unlock()
			} else {
				state.Lock()

				state.final = append(final, '\n')
				state.Unlock()
			}
		}

		state.RLock()
		finished := state.final != nil || state.failure != ""
		state.RUnlock()

		if finished {
			timer := time.NewTimer(config.FinalGrace)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}

			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			return server.Shutdown(shutdownContext)
		}
	}
}
