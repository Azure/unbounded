// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"10Gi": 10 << 30, "256Mi": 256 << 20, "1G": 1e9, "4096": 4096, "3K": 3000}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}

	for _, bad := range []string{"", "Gi", "-1", "x"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) succeeded, want error", bad)
		}
	}
}

func TestWriteLayerTarDeterministicAndValid(t *testing.T) {
	var a, b, c bytes.Buffer
	if err := writeLayerTar(&a, "s1", 0, 100_000); err != nil {
		t.Fatal(err)
	}

	_ = writeLayerTar(&b, "s1", 0, 100_000)
	_ = writeLayerTar(&c, "s2", 0, 100_000)

	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same seed produced different layers")
	}

	if bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Fatal("different seeds produced identical layers")
	}

	tr := tar.NewReader(&a)

	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}

	n, _ := io.Copy(io.Discard, tr)
	if hdr.Size != 100_000 || n != 100_000 {
		t.Fatalf("size hdr=%d read=%d", hdr.Size, n)
	}

	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("want single entry, got %v", err)
	}
}

// fakeRegistry implements the subset of the distribution API used by gen-image.
type fakeRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	uploads   map[string]*bytes.Buffer
	manifests map[string][]byte
	next      int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, uploads: map[string]*bytes.Buffer{}, manifests: map[string][]byte{}}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p := r.URL.Path

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/blobs/uploads/"):
		f.next++
		id := fmt.Sprint(f.next)
		f.uploads[id] = &bytes.Buffer{}
		w.Header().Set("Location", p+id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch && strings.Contains(p, "/blobs/uploads/"):
		id := p[strings.LastIndex(p, "/")+1:]
		_, _ = io.Copy(f.uploads[id], r.Body)

		w.Header().Set("Location", p)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(p, "/blobs/uploads/"):
		id := p[strings.LastIndex(p, "/")+1:]
		data := f.uploads[id].Bytes()
		sum := sha256.Sum256(data)

		if d := "sha256:" + hex.EncodeToString(sum[:]); d != r.URL.Query().Get("digest") {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}

		f.blobs[r.URL.Query().Get("digest")] = data

		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodHead && strings.Contains(p, "/blobs/"):
		if _, ok := f.blobs[p[strings.LastIndex(p, "/")+1:]]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodGet && strings.Contains(p, "/manifests/"):
		b, ok := f.manifests[p[strings.LastIndex(p, "/")+1:]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		sum := sha256.Sum256(b)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		_, _ = w.Write(b)
	case r.Method == http.MethodPut && strings.Contains(p, "/manifests/"):
		b, _ := io.ReadAll(r.Body)
		f.manifests[p[strings.LastIndex(p, "/")+1:]] = b
		sum := sha256.Sum256(b)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestGenImagePushesConsistentImage(t *testing.T) {
	reg := newFakeRegistry()
	srv := httptest.NewServer(reg)

	defer srv.Close()

	if err := runGenImage([]string{"--registry", srv.URL, "--repo", "bench/t", "--tag", "x", "--layers", "3", "--layer-size", "64Ki"}); err != nil {
		t.Fatal(err)
	}

	var m manifest
	if err := json.Unmarshal(reg.manifests["x"], &m); err != nil {
		t.Fatal(err)
	}

	if len(m.Layers) != 3 || m.MediaType != mediaTypeManifest {
		t.Fatalf("manifest %+v", m)
	}

	for _, l := range m.Layers {
		if int64(len(reg.blobs[l.Digest])) != l.Size || l.MediaType != mediaTypeLayerTar {
			t.Fatalf("layer %+v not stored consistently", l)
		}
	}

	if _, ok := reg.blobs[m.Config.Digest]; !ok {
		t.Fatal("config blob missing")
	}

	if err := runImageInfo([]string{"--registry", srv.URL, "--repo", "bench/t", "--tag", "x"}); err != nil {
		t.Fatal(err)
	}

	if err := runImageInfo([]string{"--registry", srv.URL, "--repo", "bench/t", "--tag", "missing"}); err == nil {
		t.Fatal("want error for missing tag")
	}
}

func TestGenImageRequiresTag(t *testing.T) {
	if err := runGenImage(nil); err == nil {
		t.Fatal("want error without --tag")
	}
}

func TestPercentile(t *testing.T) {
	v := []float64{5, 1, 4, 2, 3}
	if percentile(v, 0) != 1 || percentile(v, 50) != 3 || percentile(v, 100) != 5 {
		t.Fatal("bad percentiles")
	}

	if !math.IsNaN(percentile(nil, 50)) {
		t.Fatal("empty should be NaN")
	}
}

func TestParsePromText(t *testing.T) {
	m := map[string]float64{}
	in := "# HELP x\ngantry_origin_bytes_total 10\ngantry_mirror_bytes_served_total{source=\"peer\"} 5 1700000000\nbad line\n"

	for range 2 {
		if err := parsePromText(strings.NewReader(in), m); err != nil {
			t.Fatal(err)
		}
	}

	if m["gantry_origin_bytes_total"] != 20 || m[`gantry_mirror_bytes_served_total{source="peer"}`] != 10 {
		t.Fatalf("got %v", m)
	}
}

func TestInterestingMetric(t *testing.T) {
	for k, want := range map[string]bool{
		"gantry_origin_bytes_total":             true,
		"gantry_racer_peers":                    true,
		"gantry_racer_fetch_seconds_bucket{le}": false,
		"go_gc_duration_seconds":                false,
		"gantry_containerd_lease_active":        false,
	} {
		if interestingMetric(k) != want {
			t.Errorf("interestingMetric(%q) != %v", k, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sampleLine(node string, sec, rx, tx, busy, total, gTicks int64) string {
	g := fmt.Sprintf("%d 1024", gTicks)
	if gTicks < 0 {
		g = "- -"
	}

	return fmt.Sprintf("SAMPLE %s %d %d %d 0 %d %d 1000 500 %s 50 2048\n", node, sec*1e9, rx, tx, busy, total, g)
}

func TestSummarizeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "meta.env"), "LABEL=t1\nIMAGE=reg/x:y\nIMAGE_BYTES=1000000000\nTARGET_NODES=n1 n2 n3 n4\nTAIL_SECONDS=2\n")

	// n1: 10 s pull, 100 MB/s rx; gantry uses 1 core.
	var b strings.Builder
	for i := int64(0); i <= 12; i++ {
		b.WriteString(sampleLine("n1", 100+i, i*100e6, i*10e6, i*50, i*100, i*100))
	}

	b.WriteString("RESULT n1 100000000000 110000000000 0\n")
	writeFile(t, filepath.Join(dir, "pods", "n1.log"), b.String())

	// n2: 20 s pull, no gantry process.
	b.Reset()

	for i := int64(0); i <= 22; i++ {
		b.WriteString(sampleLine("n2", 100+i, i*50e6, 0, 0, i*100, -1))
	}

	b.WriteString("SAMPLE n2 truncated\n")
	b.WriteString("RESULT n2 100000000000 120000000000 0\n")
	writeFile(t, filepath.Join(dir, "pods", "n2.log"), b.String())
	writeFile(t, filepath.Join(dir, "pods", "n3.log"), "RESULT n3 100000000000 101000000000 1\n")
	writeFile(t, filepath.Join(dir, "origin-pre.txt"), "0 1000\n")
	writeFile(t, filepath.Join(dir, "origin-post.txt"), "0 2000001000\n")
	writeFile(t, filepath.Join(dir, "metrics-pre", "p1.txt"), "gantry_origin_bytes_total 5\n")
	writeFile(t, filepath.Join(dir, "metrics-post", "p1.txt"), "gantry_origin_bytes_total 105\ngantry_racer_x 3\n")

	s, results, err := summarize(dir)
	if err != nil {
		t.Fatal(err)
	}

	if s.Succeeded != 2 || s.Failed != 1 || len(s.MissingNodes) != 1 || s.MissingNodes[0] != "n4" {
		t.Fatalf("counts: %+v", s)
	}

	if s.PullSeconds.Min != 10 || s.PullSeconds.Max != 20 || s.MakespanSeconds != 20 {
		t.Fatalf("durations: %+v makespan %v", s.PullSeconds, s.MakespanSeconds)
	}

	if math.Abs(s.AggregateGBps-0.1) > 1e-9 || s.OriginEgressXImage != 2 {
		t.Fatalf("agg %v egress x %v", s.AggregateGBps, s.OriginEgressXImage)
	}

	if math.Abs(s.NICRxAvgMBps.Max-100) > 1e-6 || math.Abs(s.NICRxAvgMBps.Min-50) > 1e-6 {
		t.Fatalf("rx %+v", s.NICRxAvgMBps)
	}

	if s.GantryCPUAvgCores == nil || s.GantryCPUAvgCores.N != 1 || math.Abs(s.GantryCPUAvgCores.Mean-1) > 1e-9 {
		t.Fatalf("gantry cpu %+v", s.GantryCPUAvgCores)
	}

	if s.MetricDeltas["gantry_origin_bytes_total"] != 100 || s.MetricDeltas["gantry_racer_x"] != 3 {
		t.Fatalf("deltas %v", s.MetricDeltas)
	}

	var csv bytes.Buffer
	completionCSV(&csv, results)

	if csv.String() != "seconds,completed\n10.000,1\n20.000,2\n" {
		t.Fatalf("csv %q", csv.String())
	}

	var md, cmp bytes.Buffer
	writeMarkdown(&md, s)
	writeCompare(&cmp, []*Summary{s, s})

	if !strings.Contains(md.String(), "Missing: n4") || !strings.Contains(cmp.String(), "| makespan s | 20.0 | 20.0 |") {
		t.Fatalf("md:\n%s\ncmp:\n%s", md.String(), cmp.String())
	}
}

func TestSummarizeMissingMeta(t *testing.T) {
	if _, _, err := summarize(t.TempDir()); err == nil {
		t.Fatal("want error without meta.env")
	}
}

func TestParseMemSampleLine(t *testing.T) {
	node, m, err := parseMemSampleLine(strings.Fields("MEMSAMPLE n1 5 100 1000 300 600 200 400 10 20"))
	if err != nil || node != "n1" {
		t.Fatalf("parse: %q %v", node, err)
	}

	if !m.hasDisk || !m.hasCgroup || m.diskRdSectors != 100 || m.workingSet() != 600 || m.dirty != 10 {
		t.Fatalf("sample: %+v", m)
	}

	_, m, err = parseMemSampleLine(strings.Fields("MEMSAMPLE n1 5 - - - - - - - -"))
	if err != nil || m.hasDisk || m.hasCgroup {
		t.Fatalf("dashes: %+v %v", m, err)
	}

	for _, bad := range []string{"MEMSAMPLE n1 5 1", "MEMSAMPLE n1 - 1 2 3 4 5 6 7 8", "MEMSAMPLE n1 5 x 2 3 4 5 6 7 8"} {
		if _, _, err := parseMemSampleLine(strings.Fields(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSummarizeMemSamples(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "meta.env"), "LABEL=t1\nIMAGE=reg/x:y\nIMAGE_BYTES=1000000000\nTARGET_NODES=n1 n2\nTAIL_SECONDS=2\n")

	// n1: working set (current - inactive_file) peaks at 300MiB during the
	// pull and is 100MiB at the end of the tail; 2048 sectors read per s.
	var b strings.Builder

	for i := int64(0); i <= 12; i++ {
		b.WriteString(sampleLine("n1", 100+i, i*100e6, 0, i*50, i*100, i*100))

		cur, inactive, file := int64(200<<20), int64(100<<20), int64(150<<20)
		if i == 5 {
			cur, file = 400<<20, 250<<20
		}

		fmt.Fprintf(&b, "MEMSAMPLE n1 %d %d %d %d %d 0 %d 0 0\n", (100+i)*1e9, i*2048, cur, 50<<20, file, inactive)
		// 1MiB read and 4KiB written per second (cgroup); the process
		// counters are absent.
		fmt.Fprintf(&b, "IOSAMPLE n1 %d %d %d - -\n", (100+i)*1e9, i<<20, i*4096)
	}

	b.WriteString("IOSAMPLE n1 truncated\n")

	b.WriteString("MEMSAMPLE n1 truncated\n")
	b.WriteString("RESULT n1 100000000000 110000000000 0\n")
	writeFile(t, filepath.Join(dir, "pods", "n1.log"), b.String())

	// n2 has no MEMSAMPLE lines.
	b.Reset()

	for i := int64(0); i <= 12; i++ {
		b.WriteString(sampleLine("n2", 100+i, i*100e6, 0, i*50, i*100, i*100))
	}

	b.WriteString("RESULT n2 100000000000 110000000000 0\n")
	writeFile(t, filepath.Join(dir, "pods", "n2.log"), b.String())
	writeFile(t, filepath.Join(dir, "origin-pre.txt"), "0 0\n")
	writeFile(t, filepath.Join(dir, "origin-post.txt"), "0 1000000000\n")

	s, _, err := summarize(dir)
	if err != nil {
		t.Fatal(err)
	}

	if s.GantryWSPeakMiB == nil || s.GantryWSPeakMiB.N != 1 || s.GantryWSPeakMiB.Max != 300 || s.GantryWSEndMiB.Max != 100 {
		t.Fatalf("working set: %+v %+v", s.GantryWSPeakMiB, s.GantryWSEndMiB)
	}

	if s.GantryFilePeakMiB.Max != 250 || s.GantryFileEndMiB.Max != 150 || s.GantryAnonPeakMiB.Max != 50 {
		t.Fatalf("page cache: %+v %+v", s.GantryFilePeakMiB, s.GantryFileEndMiB)
	}

	if s.GantryCgReadMiB == nil || s.GantryCgReadMiB.N != 1 || s.GantryCgReadMiB.Max != 12 ||
		math.Abs(s.GantryCgWriteMiB.Max-12*4096.0/(1<<20)) > 1e-9 {
		t.Fatalf("gantry cgroup io: %+v %+v", s.GantryCgReadMiB, s.GantryCgWriteMiB)
	}

	if s.GantryProcReadMiB != nil || s.GantryProcWriteMiB != nil {
		t.Fatalf("process io without data: %+v", s.GantryProcReadMiB)
	}

	// Samples at 100..112 s are in the window: 12 intervals of 2048 sectors.
	if want := float64(12*2048*512) / 1e9; s.DiskReadGB == nil || math.Abs(s.DiskReadGB.Max-want) > 1e-9 {
		t.Fatalf("disk read: %+v, want %v", s.DiskReadGB, want)
	}
}

func TestParseIOSampleLine(t *testing.T) {
	node, m, err := parseIOSampleLine(strings.Fields("IOSAMPLE n1 5 10 20 30 40"))
	if err != nil || node != "n1" || m.ts != 5 || m.cgRead != 10 || m.cgWrite != 20 || m.procRead != 30 || m.procWrite != 40 ||
		!m.hasCgroup || !m.hasProcess {
		t.Fatalf("parse: %q %+v %v", node, m, err)
	}

	_, m, err = parseIOSampleLine(strings.Fields("IOSAMPLE n1 5 - - 1 2"))
	if err != nil || m.hasCgroup || !m.hasProcess {
		t.Fatalf("dashes: %+v %v", m, err)
	}

	for _, bad := range []string{"IOSAMPLE n1 5 1", "IOSAMPLE n1 - 1 2 3 4", "IOSAMPLE n1 5 x 2 3 4"} {
		if _, _, err := parseIOSampleLine(strings.Fields(bad)); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}

	// Deltas cover only samples in the window; a counter that goes
	// backwards (a restarted container) counts as zero.
	st := computeIO([]ioSample{
		{ts: 1, cgRead: 100, cgWrite: 100, hasCgroup: true},
		{ts: 5, cgRead: 150, cgWrite: 50, hasCgroup: true},
		{ts: 99, cgRead: 1000, cgWrite: 1000, hasCgroup: true},
	}, 0, 10)
	if !st.hasCgroup || st.cgRead != 50 || st.cgWrite != 0 || st.hasProcess {
		t.Fatalf("computeIO: %+v", st)
	}
}
