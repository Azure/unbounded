// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// Metric names racer exposes that the control plane acts on. This is the whole
// feedback channel: racer has no status file, no callback and no API, so every
// sequenced operation in R6 is gated on one of these.
const (
	metricConfigGeneration = "racer_config_generation"
	metricConfigRejected   = "racer_config_rejected_total"
	metricHealReplaying    = "racer_heal_groups_replaying"
	metricHealShedding     = "racer_heal_groups_shedding"
	metricAllocUnbacked    = "racer_alloc_unbacked_pages"
	metricAllocPressured   = "racer_alloc_cores_pressured"
	metricPaxosUnavailable = "racer_paxos_groups_unavailable"
	metricGatewayFallback  = "racer_gateway_fallback_total"
	metricExtentLivePages  = "racer_extent_live_pages"
	metricExtentTombstones = "racer_extent_tombstones"
)

// maxMetricsBody caps how much of racer's response is read. The endpoint is
// bounded by MaxExtents series of about 60 bytes each plus a fixed preamble, so
// 4 MiB is generous by two orders of magnitude and still refuses to let a
// misconfigured URL exhaust memory.
const maxMetricsBody = 4 << 20

// Sample is one parsed Prometheus series: a metric name, its labels and its
// value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Scraper reads racer's Prometheus endpoint.
//
// The endpoint is unauthenticated plaintext and serves one connection at a
// time, which is why the client below disables keep-alives and why the scrape
// interval is measured in seconds rather than milliseconds: holding the socket
// open would lock out anything else that needs it.
type Scraper struct {
	url    string
	client *http.Client
}

// NewScraper builds a scraper for racer's metrics endpoint.
func NewScraper(url string, timeout time.Duration) *Scraper {
	return &Scraper{
		url: url,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives:   true,
				MaxIdleConnsPerHost: 1,
			},
		},
	}
}

// Scrape fetches and parses the endpoint.
func (s *Scraper) Scrape(ctx context.Context) ([]Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build metrics request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", s.url, err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetricsBody)) //nolint:errcheck
		_ = resp.Body.Close()                                                 //nolint:errcheck
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %s", s.url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricsBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.url, err)
	}

	return ParseSamples(string(body)), nil
}

// ParseSamples parses the Prometheus text exposition format.
//
// This is deliberately a small hand-rolled parser rather than a dependency:
// racer emits a fixed, known set of counters and gauges with at most two
// labels, none of which use the exotic corners of the format (no histograms, no
// summaries, no escaped label values, no timestamps). Anything it does not
// understand is skipped rather than failing the scrape, because a scrape that
// fails wholesale would stall every sequenced operation.
func ParseSamples(body string) []Sample {
	var samples []Sample

	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		sample, ok := parseSample(line)
		if !ok {
			continue
		}

		samples = append(samples, sample)
	}

	return samples
}

func parseSample(line string) (Sample, bool) {
	// The value is whatever follows the last space, and the series identifier
	// is everything before it. Splitting from the right keeps label values
	// containing spaces intact.
	cut := strings.LastIndexByte(line, ' ')
	if cut < 0 {
		return Sample{}, false
	}

	series := strings.TrimSpace(line[:cut])

	value, err := strconv.ParseFloat(strings.TrimSpace(line[cut+1:]), 64)
	if err != nil || math.IsNaN(value) {
		return Sample{}, false
	}

	name := series
	labels := map[string]string{}

	if brace := strings.IndexByte(series, '{'); brace >= 0 {
		if !strings.HasSuffix(series, "}") {
			return Sample{}, false
		}

		name = series[:brace]
		labels = parseLabels(series[brace+1 : len(series)-1])
	}

	if name == "" {
		return Sample{}, false
	}

	return Sample{Name: name, Labels: labels, Value: value}, true
}

func parseLabels(raw string) map[string]string {
	labels := map[string]string{}

	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}

		labels[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}

	return labels
}

// Observation is the digest of one scrape: the node-wide health numbers and the
// per-extent live page and tombstone counts. These are exactly the values the
// operator's sequencers need, and nothing else from the endpoint is published.
type Observation struct {
	Health racerctrl.Health
	Live   map[uint32]racerctrl.LiveExtent
}

// Digest folds a scrape into the values published as Node annotations.
//
// Counter-valued metrics that carry a label (gateway fallbacks, pressured
// cores) are summed across their label values: the sequencers only ever ask
// whether the number is zero, and which reason it was is a debugging question
// answered by scraping racer directly.
func Digest(samples []Sample) Observation {
	obs := Observation{Live: map[uint32]racerctrl.LiveExtent{}}

	for _, s := range samples {
		switch s.Name {
		case metricConfigGeneration:
			obs.Health.Generation = toUint(s.Value)
		case metricConfigRejected:
			obs.Health.RejectedTotal = toUint(s.Value)
		case metricHealReplaying:
			obs.Health.Replaying = toUint(s.Value)
		case metricHealShedding:
			obs.Health.Shedding = toUint(s.Value)
		case metricAllocUnbacked:
			obs.Health.UnbackedPages = toUint(s.Value)
		case metricPaxosUnavailable:
			obs.Health.GroupsUnavail = toUint(s.Value)
		case metricAllocPressured:
			obs.Health.CoresPressured += toUint(s.Value)
		case metricGatewayFallback:
			obs.Health.GatewayFallback += toUint(s.Value)
		case metricExtentLivePages:
			if id, ok := extentLabel(s); ok {
				live := obs.Live[id]
				live.Pages = toUint(s.Value)
				obs.Live[id] = live
			}
		case metricExtentTombstones:
			if id, ok := extentLabel(s); ok {
				live := obs.Live[id]
				live.Tombstones = toUint(s.Value)
				obs.Live[id] = live
			}
		}
	}

	return obs
}

func extentLabel(s Sample) (uint32, bool) {
	raw, ok := s.Labels["extent"]
	if !ok {
		return 0, false
	}

	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || id == 0 {
		return 0, false
	}

	return uint32(id), true
}

// toUint clamps a Prometheus float to a non-negative integer. Every metric the
// control plane reads is a count, so a negative or infinite value is nonsense.
//
// Zero is not a safe reading. It is what an absent metric looks like, and every
// gate in internal/racerctrl asks whether a count has reached zero, so a value
// that failed to parse reads exactly like a sequence that has finished. That is
// what the gates guard against by requiring the node to have loaded the
// generation the count is being read against, rather than by trusting any
// number that arrives here.
func toUint(v float64) uint64 {
	if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}

	if v >= math.MaxUint64 {
		return math.MaxUint64
	}

	return uint64(v)
}
