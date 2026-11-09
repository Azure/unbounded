// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// clkTck is USER_HZ on the cluster nodes (getconf CLK_TCK), used to convert
// /proc tick counters to CPU seconds.
const clkTck = 100.0

// sample is one SAMPLE line emitted by node.sh once per interval.
type sample struct {
	ts                                 int64 // unix ns
	rx, tx                             int64 // bytes on the primary NIC
	diskWrSectors                      int64
	cpuBusy, cpuTotal                  int64 // ticks across all CPUs
	memTotalKB, memAvailKB             int64
	gantryTicks, gantryRSSKB           int64
	containerdTicks, containerdRSSKB   int64
	hasGantry, hasContainerd, hasValid bool
}

// memSample is one MEMSAMPLE line: disk reads and the gantry container's
// cgroup v2 memory.
type memSample struct {
	ts            int64
	diskRdSectors int64
	hasDisk       bool
	// cgroup memory.current and memory.stat values in bytes.
	current, anon, file, activeFile, inactiveFile, dirty, writeback int64
	hasCgroup                                                       bool
}

// workingSet is container_memory_working_set_bytes as cAdvisor computes
// it: usage minus inactive file pages.
func (m memSample) workingSet() int64 { return max(m.current-m.inactiveFile, 0) }

type nodeResult struct {
	Node    string `json:"node"`
	StartNS int64  `json:"start_ns"`
	EndNS   int64  `json:"end_ns"`
	RC      int    `json:"rc"`
	samples []sample
	mem     []memSample
}

func (n nodeResult) ok() bool { return n.RC == 0 && n.EndNS > n.StartNS }

func (n nodeResult) seconds() float64 { return float64(n.EndNS-n.StartNS) / 1e9 }

// parseSampleLine parses "SAMPLE node ts rx tx dw busy total memT memA gT gR cT cR".
// Missing processes are reported as "-".
func parseSampleLine(fields []string) (string, sample, error) {
	if len(fields) != 14 {
		return "", sample{}, fmt.Errorf("SAMPLE: want 14 fields, got %d", len(fields))
	}

	var s sample

	vals := make([]int64, 12)
	present := make([]bool, 12)

	for i, f := range fields[2:] {
		if f == "-" {
			continue
		}

		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return "", sample{}, fmt.Errorf("SAMPLE field %d: %w", i+2, err)
		}

		vals[i], present[i] = v, true
	}

	s.ts, s.rx, s.tx, s.diskWrSectors = vals[0], vals[1], vals[2], vals[3]
	s.cpuBusy, s.cpuTotal, s.memTotalKB, s.memAvailKB = vals[4], vals[5], vals[6], vals[7]
	s.gantryTicks, s.gantryRSSKB = vals[8], vals[9]
	s.containerdTicks, s.containerdRSSKB = vals[10], vals[11]
	s.hasGantry = present[8] && present[9]
	s.hasContainerd = present[10] && present[11]
	s.hasValid = present[0]

	return fields[1], s, nil
}

// parseMemSampleLine parses "MEMSAMPLE node ts diskRd current anon file
// activeFile inactiveFile dirty writeback". Missing values are "-".
func parseMemSampleLine(fields []string) (string, memSample, error) {
	if len(fields) != 11 {
		return "", memSample{}, fmt.Errorf("MEMSAMPLE: want 11 fields, got %d", len(fields))
	}

	vals := make([]int64, 9)
	present := make([]bool, 9)

	for i, f := range fields[2:] {
		if f == "-" {
			continue
		}

		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return "", memSample{}, fmt.Errorf("MEMSAMPLE field %d: %w", i+2, err)
		}

		vals[i], present[i] = v, true
	}

	if !present[0] {
		return "", memSample{}, errors.New("MEMSAMPLE: missing timestamp")
	}

	m := memSample{ts: vals[0], diskRdSectors: vals[1], hasDisk: present[1]}
	m.current, m.anon, m.file, m.activeFile = vals[2], vals[3], vals[4], vals[5]
	m.inactiveFile, m.dirty, m.writeback = vals[6], vals[7], vals[8]
	m.hasCgroup = present[2] && present[3] && present[4] && present[5] && present[6]

	return fields[1], m, nil
}

// parseNodeLog reads RESULT, SAMPLE, and MEMSAMPLE lines from one pod log.
func parseNodeLog(r io.Reader, fallbackNode string) (nodeResult, bool, error) {
	res := nodeResult{Node: fallbackNode, RC: -1}
	found := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}

		switch f[0] {
		case "RESULT":
			if len(f) != 5 {
				return res, found, fmt.Errorf("bad RESULT line %q", sc.Text())
			}

			res.Node = f[1]

			var err1, err2, err3 error

			res.StartNS, err1 = strconv.ParseInt(f[2], 10, 64)
			res.EndNS, err2 = strconv.ParseInt(f[3], 10, 64)

			res.RC, err3 = strconv.Atoi(f[4])
			if err := errors.Join(err1, err2, err3); err != nil {
				return res, found, fmt.Errorf("bad RESULT line %q: %w", sc.Text(), err)
			}

			found = true
		case "SAMPLE":
			_, s, err := parseSampleLine(f)
			if err != nil {
				continue // tolerate a truncated last line
			}

			if s.hasValid {
				res.samples = append(res.samples, s)
			}
		case "MEMSAMPLE":
			_, m, err := parseMemSampleLine(f)
			if err != nil {
				continue // tolerate a truncated last line
			}

			res.mem = append(res.mem, m)
		}
	}

	return res, found, sc.Err()
}

// percentile returns the nearest-rank percentile p (0-100) of v.
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}

	s := append([]float64(nil), v...)
	sort.Float64s(s)

	if p <= 0 {
		return s[0]
	}

	idx := int(math.Ceil(p/100*float64(len(s)))) - 1

	return s[max(0, min(idx, len(s)-1))]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}

	t := 0.0
	for _, x := range v {
		t += x
	}

	return t / float64(len(v))
}

// Dist is a distribution summary across nodes.
type Dist struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

func dist(v []float64) Dist {
	return Dist{
		N: len(v), Min: percentile(v, 0), P50: percentile(v, 50), P90: percentile(v, 90),
		P99: percentile(v, 99), Max: percentile(v, 100), Mean: mean(v),
	}
}

// windowStats computes per-node rates over samples within [from, to] ns.
type windowStats struct {
	rxAvg, rxPeak, txAvg, txPeak       float64 // bytes/s
	diskWrBytes                        float64
	cpuUtilAvg, cpuUtilPeak            float64 // fraction 0-1
	gantryCPUAvg, gantryCPUPeak        float64 // cores
	gantryRSSAvg, gantryRSSPeak        float64 // bytes
	containerdCPUAvg, containerdCPUPk  float64
	containerdRSSAvg, containerdRSSPk  float64
	memUsedPeak                        float64 // bytes
	hasGantry, hasContainerd, hasRates bool
}

func computeWindow(samples []sample, from, to int64) windowStats {
	var w windowStats

	var in []sample

	for _, s := range samples {
		if s.ts >= from && s.ts <= to {
			in = append(in, s)
		}
	}

	if len(in) < 2 {
		return w
	}

	w.hasRates = true
	first, last := in[0], in[len(in)-1]
	dt := float64(last.ts-first.ts) / 1e9
	w.rxAvg = float64(last.rx-first.rx) / dt
	w.txAvg = float64(last.tx-first.tx) / dt
	w.diskWrBytes = float64(last.diskWrSectors-first.diskWrSectors) * 512

	if tot := last.cpuTotal - first.cpuTotal; tot > 0 {
		w.cpuUtilAvg = float64(last.cpuBusy-first.cpuBusy) / float64(tot)
	}

	var gCPU, cCPU, gRSS, cRSS []float64

	for i := 1; i < len(in); i++ {
		a, b := in[i-1], in[i]
		d := float64(b.ts-a.ts) / 1e9

		if d <= 0 {
			continue
		}

		w.rxPeak = math.Max(w.rxPeak, float64(b.rx-a.rx)/d)
		w.txPeak = math.Max(w.txPeak, float64(b.tx-a.tx)/d)

		if tot := b.cpuTotal - a.cpuTotal; tot > 0 {
			w.cpuUtilPeak = math.Max(w.cpuUtilPeak, float64(b.cpuBusy-a.cpuBusy)/float64(tot))
		}

		w.memUsedPeak = math.Max(w.memUsedPeak, float64(b.memTotalKB-b.memAvailKB)*1024)

		// A tick decrease means the process restarted; skip that interval.
		if a.hasGantry && b.hasGantry && b.gantryTicks >= a.gantryTicks {
			gCPU = append(gCPU, float64(b.gantryTicks-a.gantryTicks)/clkTck/d)
			gRSS = append(gRSS, float64(b.gantryRSSKB)*1024)
		}

		if a.hasContainerd && b.hasContainerd && b.containerdTicks >= a.containerdTicks {
			cCPU = append(cCPU, float64(b.containerdTicks-a.containerdTicks)/clkTck/d)
			cRSS = append(cRSS, float64(b.containerdRSSKB)*1024)
		}
	}

	if len(gCPU) > 0 {
		w.hasGantry = true
		w.gantryCPUAvg, w.gantryCPUPeak = mean(gCPU), percentile(gCPU, 100)
		w.gantryRSSAvg, w.gantryRSSPeak = mean(gRSS), percentile(gRSS, 100)
	}

	if len(cCPU) > 0 {
		w.hasContainerd = true
		w.containerdCPUAvg, w.containerdCPUPk = mean(cCPU), percentile(cCPU, 100)
		w.containerdRSSAvg, w.containerdRSSPk = mean(cRSS), percentile(cRSS, 100)
	}

	return w
}

// memStats summarizes MEMSAMPLE lines within a window.
type memStats struct {
	diskRdBytes                  float64
	hasDisk                      bool
	wsPeak, wsEnd, anonPeak      float64 // bytes
	filePeak, fileEnd, dirtyPeak float64 // bytes
	hasCgroup                    bool
}

func computeMem(mem []memSample, from, to int64) memStats {
	var (
		ms        memStats
		firstDisk *memSample
		lastDisk  *memSample
		lastCg    *memSample
	)

	for i := range mem {
		m := &mem[i]
		if m.ts < from || m.ts > to {
			continue
		}

		if m.hasDisk {
			if firstDisk == nil {
				firstDisk = m
			}

			lastDisk = m
		}

		if m.hasCgroup {
			ms.hasCgroup = true
			ms.wsPeak = math.Max(ms.wsPeak, float64(m.workingSet()))
			ms.anonPeak = math.Max(ms.anonPeak, float64(m.anon))
			ms.filePeak = math.Max(ms.filePeak, float64(m.file))
			ms.dirtyPeak = math.Max(ms.dirtyPeak, float64(m.dirty))
			lastCg = m
		}
	}

	if firstDisk != nil && lastDisk != firstDisk {
		ms.hasDisk = true
		ms.diskRdBytes = float64(lastDisk.diskRdSectors-firstDisk.diskRdSectors) * 512
	}

	if lastCg != nil {
		ms.wsEnd, ms.fileEnd = float64(lastCg.workingSet()), float64(lastCg.file)
	}

	return ms
}

// Summary is the machine-readable result of one benchmark run.
type Summary struct {
	Label              string             `json:"label"`
	Image              string             `json:"image"`
	ImageBytes         int64              `json:"image_bytes"`
	NodesExpected      int                `json:"nodes_expected"`
	NodesReported      int                `json:"nodes_reported"`
	Succeeded          int                `json:"succeeded"`
	Failed             int                `json:"failed"`
	FailedNodes        []string           `json:"failed_nodes,omitempty"`
	MissingNodes       []string           `json:"missing_nodes,omitempty"`
	PullSeconds        Dist               `json:"pull_seconds"`
	MakespanSeconds    float64            `json:"makespan_seconds"`
	AggregateGBps      float64            `json:"aggregate_gbps"` // GB/s (1e9), bytes*succeeded/makespan
	PerNodeMBps        Dist               `json:"per_node_mbps"`  // MB/s (1e6), image bytes/pull duration
	OriginEgressBytes  int64              `json:"origin_egress_bytes"`
	OriginEgressXImage float64            `json:"origin_egress_x_image"`
	OriginTxPeakMBps   float64            `json:"origin_tx_peak_mbps"`
	OriginTxAvgMBps    float64            `json:"origin_tx_avg_mbps"`
	NICRxAvgMBps       Dist               `json:"nic_rx_avg_mbps"`
	NICRxPeakMBps      Dist               `json:"nic_rx_peak_mbps"`
	NICTxAvgMBps       Dist               `json:"nic_tx_avg_mbps"`
	NICTxPeakMBps      Dist               `json:"nic_tx_peak_mbps"`
	DiskWriteGB        Dist               `json:"disk_write_gb"`
	NodeCPUUtilAvgPct  Dist               `json:"node_cpu_util_avg_pct"`
	NodeCPUUtilPeakPct Dist               `json:"node_cpu_util_peak_pct"`
	NodeMemUsedPeakGiB Dist               `json:"node_mem_used_peak_gib"`
	GantryCPUAvgCores  *Dist              `json:"gantry_cpu_avg_cores,omitempty"`
	GantryCPUPeakCores *Dist              `json:"gantry_cpu_peak_cores,omitempty"`
	GantryRSSAvgMiB    *Dist              `json:"gantry_rss_avg_mib,omitempty"`
	GantryRSSPeakMiB   *Dist              `json:"gantry_rss_peak_mib,omitempty"`
	CtrdCPUAvgCores    *Dist              `json:"containerd_cpu_avg_cores,omitempty"`
	CtrdCPUPeakCores   *Dist              `json:"containerd_cpu_peak_cores,omitempty"`
	CtrdRSSAvgMiB      *Dist              `json:"containerd_rss_avg_mib,omitempty"`
	CtrdRSSPeakMiB     *Dist              `json:"containerd_rss_peak_mib,omitempty"`
	DiskReadGB         *Dist              `json:"disk_read_gb,omitempty"`
	GantryWSPeakMiB    *Dist              `json:"gantry_working_set_peak_mib,omitempty"`
	GantryWSEndMiB     *Dist              `json:"gantry_working_set_end_mib,omitempty"`
	GantryAnonPeakMiB  *Dist              `json:"gantry_anon_peak_mib,omitempty"`
	GantryFilePeakMiB  *Dist              `json:"gantry_page_cache_peak_mib,omitempty"`
	GantryFileEndMiB   *Dist              `json:"gantry_page_cache_end_mib,omitempty"`
	GantryDirtyPeakMiB *Dist              `json:"gantry_dirty_peak_mib,omitempty"`
	MetricDeltas       map[string]float64 `json:"metric_deltas,omitempty"`
	MetricPodsPre      int                `json:"metric_pods_pre"`
	MetricPodsPost     int                `json:"metric_pods_post"`
}

func readMeta(path string) (map[string]string, error) {
	m := map[string]string{}

	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer closeFile(f)

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}

	return m, sc.Err()
}

// readNetdevPair reads "rx tx" from an origin pre/post file.
func readNetdevPair(path string) (int64, int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}

	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, 0, false
	}

	rx, err1 := strconv.ParseInt(f[0], 10, 64)
	tx, err2 := strconv.ParseInt(f[1], 10, 64)

	return rx, tx, err1 == nil && err2 == nil
}

// parsePromText sums every sample of the text exposition by "name{labels}".
func parsePromText(r io.Reader, into map[string]float64) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 4<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}

		// Series and value are separated by the last space outside braces;
		// an optional timestamp may follow.
		key, rest := line, ""
		if i := strings.LastIndexByte(line, '}'); i >= 0 {
			key, rest = line[:i+1], strings.TrimSpace(line[i+1:])
		} else if j := strings.IndexByte(line, ' '); j >= 0 {
			key, rest = line[:j], strings.TrimSpace(line[j+1:])
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}

		into[key] += v
	}

	return sc.Err()
}

func readMetricsDir(dir string) (map[string]float64, int, error) {
	out := map[string]float64{}

	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, 0, err
	}

	n := 0

	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			return nil, 0, err
		}

		err = parsePromText(f, out)
		closeFile(f)

		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", p, err)
		}

		n++
	}

	return out, n, nil
}

// interestingMetric selects the counters that are reported as deltas.
func interestingMetric(key string) bool {
	name, _, _ := strings.Cut(key, "{")
	if strings.HasPrefix(name, "gantry_cachemesh_") {
		return !strings.HasSuffix(name, "_bucket")
	}

	return strings.HasPrefix(name, "gantry_") && strings.HasSuffix(name, "_total")
}

// atoi64 parses an optional integer, treating empty or malformed input as 0.
func atoi64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}

	return v
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
	}
}

func ptr(d Dist) *Dist { return &d }

func summarize(dir string) (*Summary, []nodeResult, error) {
	meta, err := readMeta(filepath.Join(dir, "meta.env"))
	if err != nil {
		return nil, nil, fmt.Errorf("meta.env: %w", err)
	}

	s := &Summary{Label: meta["LABEL"], Image: meta["IMAGE"]}
	s.ImageBytes = atoi64(meta["IMAGE_BYTES"])
	expected := strings.Fields(meta["TARGET_NODES"])
	s.NodesExpected = len(expected)

	files, err := filepath.Glob(filepath.Join(dir, "pods", "*.log"))
	if err != nil {
		return nil, nil, err
	}

	var results []nodeResult

	seen := map[string]bool{}

	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			return nil, nil, err
		}

		r, found, err := parseNodeLog(f, strings.TrimSuffix(filepath.Base(p), ".log"))
		closeFile(f)

		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p, err)
		}

		if !found {
			r.RC = -1
		}

		seen[r.Node] = true
		results = append(results, r)
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Node < results[j].Node })

	for _, n := range expected {
		if !seen[n] {
			s.MissingNodes = append(s.MissingNodes, n)
		}
	}

	s.NodesReported = len(results)

	var (
		durs, tput                         []float64
		firstStart, lastEnd                int64 = math.MaxInt64, 0
		rxA, rxP, txA, txP, dw, cuA, cuP   []float64
		memP                               []float64
		gA, gP, gRA, gRP, cA, cP, cRA, cRP []float64
		dr, wsP, wsE, anP, fP, fE, dP      []float64
	)

	tail := atoi64(meta["TAIL_SECONDS"])

	for _, r := range results {
		if !r.ok() {
			s.Failed++
			s.FailedNodes = append(s.FailedNodes, fmt.Sprintf("%s(rc=%d)", r.Node, r.RC))

			continue
		}

		s.Succeeded++

		durs = append(durs, r.seconds())

		if s.ImageBytes > 0 {
			tput = append(tput, float64(s.ImageBytes)/r.seconds()/1e6)
		}

		firstStart = min(firstStart, r.StartNS)
		lastEnd = max(lastEnd, r.EndNS)

		// Rates cover the pull itself; disk writes include the tail so
		// that unpacking finished after the pull returns is counted.
		w := computeWindow(r.samples, r.StartNS-1e9, r.EndNS+1e9)
		wt := computeWindow(r.samples, r.StartNS-1e9, r.EndNS+tail*1e9)

		// Memory covers the pull and the idle tail; the end value is the
		// last sample of the tail.
		if ms := computeMem(r.mem, r.StartNS-1e9, r.EndNS+tail*1e9); ms.hasDisk || ms.hasCgroup {
			if ms.hasDisk {
				dr = append(dr, ms.diskRdBytes/1e9)
			}

			if ms.hasCgroup {
				wsP, wsE = append(wsP, ms.wsPeak/(1<<20)), append(wsE, ms.wsEnd/(1<<20))
				anP, dP = append(anP, ms.anonPeak/(1<<20)), append(dP, ms.dirtyPeak/(1<<20))
				fP, fE = append(fP, ms.filePeak/(1<<20)), append(fE, ms.fileEnd/(1<<20))
			}
		}

		if !w.hasRates {
			continue
		}

		rxA, rxP = append(rxA, w.rxAvg/1e6), append(rxP, w.rxPeak/1e6)
		txA, txP = append(txA, w.txAvg/1e6), append(txP, w.txPeak/1e6)
		dw = append(dw, wt.diskWrBytes/1e9)
		cuA, cuP = append(cuA, w.cpuUtilAvg*100), append(cuP, w.cpuUtilPeak*100)
		memP = append(memP, w.memUsedPeak/(1<<30))

		if w.hasGantry {
			gA, gP = append(gA, w.gantryCPUAvg), append(gP, w.gantryCPUPeak)
			gRA, gRP = append(gRA, w.gantryRSSAvg/(1<<20)), append(gRP, w.gantryRSSPeak/(1<<20))
		}

		if w.hasContainerd {
			cA, cP = append(cA, w.containerdCPUAvg), append(cP, w.containerdCPUPk)
			cRA, cRP = append(cRA, w.containerdRSSAvg/(1<<20)), append(cRP, w.containerdRSSPk/(1<<20))
		}
	}

	s.PullSeconds = dist(durs)
	s.PerNodeMBps = dist(tput)

	if s.Succeeded > 0 {
		s.MakespanSeconds = float64(lastEnd-firstStart) / 1e9
		s.AggregateGBps = float64(s.ImageBytes) * float64(s.Succeeded) / s.MakespanSeconds / 1e9
	}

	s.NICRxAvgMBps, s.NICRxPeakMBps = dist(rxA), dist(rxP)
	s.NICTxAvgMBps, s.NICTxPeakMBps = dist(txA), dist(txP)
	s.DiskWriteGB = dist(dw)
	s.NodeCPUUtilAvgPct, s.NodeCPUUtilPeakPct = dist(cuA), dist(cuP)
	s.NodeMemUsedPeakGiB = dist(memP)

	if len(dr) > 0 {
		s.DiskReadGB = ptr(dist(dr))
	}

	if len(wsP) > 0 {
		s.GantryWSPeakMiB, s.GantryWSEndMiB = ptr(dist(wsP)), ptr(dist(wsE))
		s.GantryAnonPeakMiB, s.GantryDirtyPeakMiB = ptr(dist(anP)), ptr(dist(dP))
		s.GantryFilePeakMiB, s.GantryFileEndMiB = ptr(dist(fP)), ptr(dist(fE))
	}

	if len(gA) > 0 {
		s.GantryCPUAvgCores, s.GantryCPUPeakCores = ptr(dist(gA)), ptr(dist(gP))
		s.GantryRSSAvgMiB, s.GantryRSSPeakMiB = ptr(dist(gRA)), ptr(dist(gRP))
	}

	if len(cA) > 0 {
		s.CtrdCPUAvgCores, s.CtrdCPUPeakCores = ptr(dist(cA)), ptr(dist(cP))
		s.CtrdRSSAvgMiB, s.CtrdRSSPeakMiB = ptr(dist(cRA)), ptr(dist(cRP))
	}

	_, preTx, okPre := readNetdevPair(filepath.Join(dir, "origin-pre.txt"))

	_, postTx, okPost := readNetdevPair(filepath.Join(dir, "origin-post.txt"))
	if okPre && okPost {
		s.OriginEgressBytes = postTx - preTx
		if s.ImageBytes > 0 {
			s.OriginEgressXImage = float64(s.OriginEgressBytes) / float64(s.ImageBytes)
		}
	}

	if f, err := os.Open(filepath.Join(dir, "origin.log")); err == nil {
		o, _, err := parseNodeLog(f, "origin")
		closeFile(f)

		if err == nil && s.Succeeded > 0 {
			w := computeWindow(o.samples, firstStart-1e9, lastEnd+1e9)
			s.OriginTxPeakMBps, s.OriginTxAvgMBps = w.txPeak/1e6, w.txAvg/1e6
		}
	}

	pre, nPre, err := readMetricsDir(filepath.Join(dir, "metrics-pre"))
	if err != nil {
		return nil, nil, err
	}

	post, nPost, err := readMetricsDir(filepath.Join(dir, "metrics-post"))
	if err != nil {
		return nil, nil, err
	}

	s.MetricPodsPre, s.MetricPodsPost = nPre, nPost

	if nPost > 0 {
		s.MetricDeltas = map[string]float64{}

		for k, v := range post {
			if !interestingMetric(k) {
				continue
			}

			if d := v - pre[k]; d != 0 {
				s.MetricDeltas[k] = d
			}
		}
	}

	return s, results, nil
}

// errWriter formats to w and keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) f(format string, args ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, args...)
	}
}

// completionCSV writes "seconds_since_first_start,completed" rows.
func completionCSV(w io.Writer, results []nodeResult) {
	p := &errWriter{w: w}

	var ends []int64

	first := int64(math.MaxInt64)

	for _, r := range results {
		if r.ok() {
			ends = append(ends, r.EndNS)
			first = min(first, r.StartNS)
		}
	}

	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	p.f("seconds,completed" + "\n")

	for i, e := range ends {
		p.f("%.3f,%d\n", float64(e-first)/1e9, i+1)
	}
}

func fmtDist(d Dist) string {
	if d.N == 0 {
		return "n/a"
	}

	return fmt.Sprintf("%.1f / %.1f / %.1f / %.1f / %.1f (mean %.1f, n=%d)", d.Min, d.P50, d.P90, d.P99, d.Max, d.Mean, d.N)
}

func fmtDistPtr(d *Dist) string {
	if d == nil {
		return "n/a"
	}

	return fmtDist(*d)
}

func writeMarkdown(w io.Writer, s *Summary) {
	p := &errWriter{w: w}

	p.f("# Gantry cluster bench: %s\n\n", s.Label)
	p.f("- Image: `%s` (%.2f GB)\n", s.Image, float64(s.ImageBytes)/1e9)
	p.f("- Nodes: expected %d, reported %d, succeeded %d, failed %d\n", s.NodesExpected, s.NodesReported, s.Succeeded, s.Failed)

	if len(s.FailedNodes) > 0 {
		p.f("- Failed: %s\n", strings.Join(s.FailedNodes, ", "))
	}

	if len(s.MissingNodes) > 0 {
		p.f("- Missing: %s\n", strings.Join(s.MissingNodes, ", "))
	}

	p.f("- Makespan: %.1f s\n", s.MakespanSeconds)
	p.f("- Aggregate throughput: %.2f GB/s\n", s.AggregateGBps)
	p.f("- Origin egress: %.2f GB (%.2fx image), peak %.0f MB/s, avg %.0f MB/s\n\n",
		float64(s.OriginEgressBytes)/1e9, s.OriginEgressXImage, s.OriginTxPeakMBps, s.OriginTxAvgMBps)

	p.f("| metric (across nodes) | min / p50 / p90 / p99 / max |" + "\n")
	p.f("|---|---|" + "\n")

	rows := []struct {
		name string
		val  string
	}{
		{"pull duration (s)", fmtDist(s.PullSeconds)},
		{"per-node throughput (MB/s)", fmtDist(s.PerNodeMBps)},
		{"NIC rx avg (MB/s)", fmtDist(s.NICRxAvgMBps)},
		{"NIC rx peak (MB/s)", fmtDist(s.NICRxPeakMBps)},
		{"NIC tx avg (MB/s)", fmtDist(s.NICTxAvgMBps)},
		{"NIC tx peak (MB/s)", fmtDist(s.NICTxPeakMBps)},
		{"disk write incl. tail (GB)", fmtDist(s.DiskWriteGB)},
		{"node CPU util avg (%)", fmtDist(s.NodeCPUUtilAvgPct)},
		{"node CPU util peak (%)", fmtDist(s.NodeCPUUtilPeakPct)},
		{"node mem used peak (GiB)", fmtDist(s.NodeMemUsedPeakGiB)},
		{"gantry CPU avg (cores)", fmtDistPtr(s.GantryCPUAvgCores)},
		{"gantry CPU peak (cores)", fmtDistPtr(s.GantryCPUPeakCores)},
		{"gantry RSS avg (MiB)", fmtDistPtr(s.GantryRSSAvgMiB)},
		{"gantry RSS peak (MiB)", fmtDistPtr(s.GantryRSSPeakMiB)},
		{"disk read incl. tail (GB)", fmtDistPtr(s.DiskReadGB)},
		{"gantry working set peak (MiB)", fmtDistPtr(s.GantryWSPeakMiB)},
		{"gantry working set at end of tail (MiB)", fmtDistPtr(s.GantryWSEndMiB)},
		{"gantry anon peak (MiB)", fmtDistPtr(s.GantryAnonPeakMiB)},
		{"gantry page cache peak (MiB)", fmtDistPtr(s.GantryFilePeakMiB)},
		{"gantry page cache at end of tail (MiB)", fmtDistPtr(s.GantryFileEndMiB)},
		{"gantry dirty page cache peak (MiB)", fmtDistPtr(s.GantryDirtyPeakMiB)},
		{"containerd CPU avg (cores)", fmtDistPtr(s.CtrdCPUAvgCores)},
		{"containerd CPU peak (cores)", fmtDistPtr(s.CtrdCPUPeakCores)},
		{"containerd RSS avg (MiB)", fmtDistPtr(s.CtrdRSSAvgMiB)},
		{"containerd RSS peak (MiB)", fmtDistPtr(s.CtrdRSSPeakMiB)},
	}
	for _, r := range rows {
		p.f("| %s | %s |\n", r.name, r.val)
	}

	if len(s.MetricDeltas) > 0 {
		p.f("\n## Gantry metric deltas (sum over %d pods)\n\n| series | delta |\n|---|---|\n", s.MetricPodsPost)

		keys := make([]string, 0, len(s.MetricDeltas))
		for k := range s.MetricDeltas {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			p.f("| `%s` | %.6g |\n", k, s.MetricDeltas[k])
		}
	}
}

func runSummarize(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 {
		return errors.New("usage: summarize <run-dir>")
	}

	dir := fs.Arg(0)

	s, results, err := summarize(dir)
	if err != nil {
		return err
	}

	js, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "summary.json"), append(js, '\n'), 0o644); err != nil {
		return err
	}

	csvf, err := os.Create(filepath.Join(dir, "completion.csv"))
	if err != nil {
		return err
	}

	completionCSV(csvf, results)

	if err := csvf.Close(); err != nil {
		return err
	}

	md, err := os.Create(filepath.Join(dir, "summary.md"))
	if err != nil {
		return err
	}

	writeMarkdown(io.MultiWriter(md, os.Stdout), s)

	return md.Close()
}

func runCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() < 2 {
		return errors.New("usage: compare <summary.json|run-dir> <summary.json|run-dir> [more]")
	}

	var sums []*Summary

	for _, p := range fs.Args() {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			p = filepath.Join(p, "summary.json")
		}

		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		var s Summary
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}

		sums = append(sums, &s)
	}

	writeCompare(os.Stdout, sums)

	return nil
}

func writeCompare(w io.Writer, sums []*Summary) {
	p := &errWriter{w: w}

	p.f("| metric |")

	for _, s := range sums {
		p.f(" %s |", s.Label)
	}

	p.f("\n|---|")

	for range sums {
		p.f("---|")
	}

	p.f("\n")

	row := func(name string, f func(*Summary) string) {
		p.f("| %s |", name)

		for _, s := range sums {
			p.f(" %s |", f(s))
		}

		p.f("\n")
	}
	num := func(format string, f func(*Summary) float64) func(*Summary) string {
		return func(s *Summary) string { return fmt.Sprintf(format, f(s)) }
	}
	dp := func(f func(*Summary) *Dist, pick func(Dist) float64) func(*Summary) string {
		return func(s *Summary) string {
			d := f(s)
			if d == nil || d.N == 0 {
				return "n/a"
			}

			return fmt.Sprintf("%.1f", pick(*d))
		}
	}
	p50 := func(d Dist) float64 { return d.P50 }
	p99 := func(d Dist) float64 { return d.P99 }
	mx := func(d Dist) float64 { return d.Max }

	row("image GB", num("%.2f", func(s *Summary) float64 { return float64(s.ImageBytes) / 1e9 }))
	row("succeeded / expected", func(s *Summary) string { return fmt.Sprintf("%d / %d", s.Succeeded, s.NodesExpected) })
	row("makespan s", num("%.1f", func(s *Summary) float64 { return s.MakespanSeconds }))
	row("pull s p50", dp(func(s *Summary) *Dist { return &s.PullSeconds }, p50))
	row("pull s p99", dp(func(s *Summary) *Dist { return &s.PullSeconds }, p99))
	row("pull s max", dp(func(s *Summary) *Dist { return &s.PullSeconds }, mx))
	row("aggregate GB/s", num("%.2f", func(s *Summary) float64 { return s.AggregateGBps }))
	row("per-node MB/s p50", dp(func(s *Summary) *Dist { return &s.PerNodeMBps }, p50))
	row("origin egress GB", num("%.2f", func(s *Summary) float64 { return float64(s.OriginEgressBytes) / 1e9 }))
	row("origin egress x image", num("%.2f", func(s *Summary) float64 { return s.OriginEgressXImage }))
	row("origin tx peak MB/s", num("%.0f", func(s *Summary) float64 { return s.OriginTxPeakMBps }))
	row("NIC rx avg MB/s p50", dp(func(s *Summary) *Dist { return &s.NICRxAvgMBps }, p50))
	row("NIC rx peak MB/s p99", dp(func(s *Summary) *Dist { return &s.NICRxPeakMBps }, p99))
	row("NIC tx avg MB/s p50", dp(func(s *Summary) *Dist { return &s.NICTxAvgMBps }, p50))
	row("NIC tx peak MB/s p99", dp(func(s *Summary) *Dist { return &s.NICTxPeakMBps }, p99))
	row("disk write GB p50", dp(func(s *Summary) *Dist { return &s.DiskWriteGB }, p50))
	row("node CPU avg % p50", dp(func(s *Summary) *Dist { return &s.NodeCPUUtilAvgPct }, p50))
	row("gantry CPU avg cores p50", dp(func(s *Summary) *Dist { return s.GantryCPUAvgCores }, p50))
	row("gantry CPU peak cores p99", dp(func(s *Summary) *Dist { return s.GantryCPUPeakCores }, p99))
	row("gantry RSS peak MiB p50", dp(func(s *Summary) *Dist { return s.GantryRSSPeakMiB }, p50))
	row("gantry RSS peak MiB p99", dp(func(s *Summary) *Dist { return s.GantryRSSPeakMiB }, p99))
	row("containerd CPU avg cores p50", dp(func(s *Summary) *Dist { return s.CtrdCPUAvgCores }, p50))
	row("containerd RSS peak MiB p99", dp(func(s *Summary) *Dist { return s.CtrdRSSPeakMiB }, p99))
	row("disk read GB p50", dp(func(s *Summary) *Dist { return s.DiskReadGB }, p50))
	row("gantry working set peak MiB p50", dp(func(s *Summary) *Dist { return s.GantryWSPeakMiB }, p50))
	row("gantry working set peak MiB p99", dp(func(s *Summary) *Dist { return s.GantryWSPeakMiB }, p99))
	row("gantry working set end MiB p50", dp(func(s *Summary) *Dist { return s.GantryWSEndMiB }, p50))
	row("gantry anon peak MiB p50", dp(func(s *Summary) *Dist { return s.GantryAnonPeakMiB }, p50))
	row("gantry page cache peak MiB p50", dp(func(s *Summary) *Dist { return s.GantryFilePeakMiB }, p50))
	row("gantry page cache end MiB p50", dp(func(s *Summary) *Dist { return s.GantryFileEndMiB }, p50))

	keys := map[string]bool{}

	for _, s := range sums {
		for k := range s.MetricDeltas {
			name, _, _ := strings.Cut(k, "{")
			if strings.Contains(name, "bytes") || strings.HasPrefix(name, "gantry_cachemesh_") {
				keys[k] = true
			}
		}
	}

	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}

	sort.Strings(sorted)

	for _, k := range sorted {
		row("`"+k+"`", func(s *Summary) string { return fmt.Sprintf("%.4g", s.MetricDeltas[k]) })
	}
}
