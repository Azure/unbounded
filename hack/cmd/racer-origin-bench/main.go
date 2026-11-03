// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// racer-origin-bench measures the CPU cost of serving cold Racer reads from an
// Azure Blob origin. It is driven by hack/scripts/racer-origin-cpu-bench.sh,
// which runs "run" as root in a private mount namespace where /run is a
// scratch bind mount, and "summarize" over the resulting JSON files.
//
// Variants:
//
//	before: racer + racer-object origin --provider azure (separate Go process
//	        on the origin UDS) reading the fake Azure Blob server.
//	after:  racer reading the fake Azure Blob server directly; the fake
//	        control publishes the Azure origin when it sees
//	        RACER_BENCH_AZURE_CONTAINER_URL.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/racerbench/fakeblob"
	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	cacheName = "gantry"
	cacheUID  = "44444444-4444-4444-8444-444444444444"
	clusterID = "11111111-1111-4111-8111-111111111111"
	account   = "devstoreaccount1"
	container = "bench"
	originUID = 65532
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: racer-origin-bench {run|summarize} [flags]")
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "run":
		err = runCommand(os.Args[2:])
	case "summarize":
		err = summarizeCommand(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "racer-origin-bench:", err)
		os.Exit(1)
	}
}

type runOptions struct {
	Variant, Mode                     string
	Racer, Control, RacerObject, Fake string
	Work, Out                         string
	ObjectSize, TotalBytes, SlabBytes int64
	Concurrency, Rep                  int
	CPUsRacer, CPUsOrigin, CPUsFake   string
	OriginGroups                      string
	MaxThreads, OriginMaxRequests     int
	PageCredits                       int
	ReadyTimeout                      time.Duration
	OriginExtraArgs                   string
}

// CPU is cumulative process CPU across all threads, including exited ones
// (utime+stime from /proc/<pid>/stat), plus run time summed over live threads
// from schedstat for cross-checking.
type CPU struct {
	UserSeconds     float64 `json:"user_seconds"`
	SystemSeconds   float64 `json:"system_seconds"`
	Seconds         float64 `json:"seconds"`
	SchedstatSecond float64 `json:"schedstat_live_threads_seconds"`
	Threads         int     `json:"threads"`
}

type Result struct {
	Variant string `json:"variant"`
	Mode    string `json:"mode"`
	// PublishedMode is the cache mode the control reported publishing; the
	// run fails unless it equals Mode.
	PublishedMode string             `json:"published_mode"`
	Rep           int                `json:"rep"`
	ObjectSize    int64              `json:"object_size"`
	Concurrency   int                `json:"concurrency"`
	Objects       int                `json:"objects"`
	Bytes         int64              `json:"bytes"`
	Seconds       float64            `json:"seconds"`
	MiBPerSecond  float64            `json:"mib_per_second"`
	CPU           map[string]CPU     `json:"cpu"`
	CPUPerGiB     map[string]float64 `json:"cpu_seconds_per_gib"`
	Fake          fakeblob.Snapshot  `json:"fake"`
	CPUSets       map[string]string  `json:"cpu_sets"`
	Binaries      map[string]string  `json:"binaries"`
	Started       time.Time          `json:"started"`
	ClockTicks    int                `json:"clock_ticks"`
	// Retries counts whole-object retries after transient errors; RetriedBytes
	// were delivered and then discarded. Both are included in CPU.
	Retries        int64  `json:"retries"`
	RetriedBytes   int64  `json:"retried_bytes"`
	LastRetryError string `json:"last_retry_error,omitempty"`
}

func runCommand(args []string) error {
	var o runOptions

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&o.Variant, "variant", "before", "before (racer-object origin) or after (native origin)")
	fs.StringVar(&o.Mode, "mode", "cached", "RACER_BENCH_MODE passed to the control (after variant): cached or passthrough")
	fs.StringVar(&o.Racer, "racer", "", "racer binary")
	fs.StringVar(&o.Control, "control", "", "racer-performance-control binary")
	fs.StringVar(&o.RacerObject, "racer-object", "", "racer-object binary (before only)")
	fs.StringVar(&o.Fake, "fake", "", "fake-azure-blob binary")
	fs.StringVar(&o.Work, "work", "", "fresh scratch directory (slabs, logs)")
	fs.StringVar(&o.Out, "out", "", "result JSON path")
	fs.Int64Var(&o.ObjectSize, "object-size", 64<<20, "object size in bytes")
	fs.Int64Var(&o.TotalBytes, "total-bytes", 4<<30, "bytes read in the load phase")
	fs.Int64Var(&o.SlabBytes, "slab-bytes", 1<<30, "RACER_SLAB_BYTES")
	fs.IntVar(&o.Concurrency, "concurrency", 8, "concurrent reads")
	fs.IntVar(&o.Rep, "rep", 0, "repetition number (recorded only)")
	fs.StringVar(&o.CPUsRacer, "cpus-racer", "0-3", "taskset CPU list for racer")
	fs.StringVar(&o.CPUsOrigin, "cpus-origin", "4-7", "taskset CPU list for racer-object")
	fs.StringVar(&o.CPUsFake, "cpus-fake", "8-11", "taskset CPU list for the fake server")
	fs.StringVar(&o.OriginGroups, "origin-groups", "", "supplementary groups for the origin (to traverse the binary path)")
	fs.IntVar(&o.MaxThreads, "max-threads", 2, "RACER_MAX_THREADS")
	fs.IntVar(&o.OriginMaxRequests, "origin-max-requests", 0, "racer-object --max-concurrent-requests (0: default)")
	fs.IntVar(&o.PageCredits, "page-credits", 0, "racersdk PageCredits (0: default)")
	fs.DurationVar(&o.ReadyTimeout, "ready-timeout", 30*time.Second, "readiness timeout")
	fs.StringVar(&o.OriginExtraArgs, "origin-args", "", "extra space-separated racer-object origin arguments")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if o.Racer == "" || o.Control == "" || o.Fake == "" || o.Work == "" || o.Out == "" ||
		(o.Variant == "before" && o.RacerObject == "") || (o.Variant != "before" && o.Variant != "after") {
		return errors.New("missing or invalid flags; see -h")
	}

	if o.ObjectSize <= 0 || o.TotalBytes < o.ObjectSize || o.Concurrency <= 0 {
		return errors.New("invalid workload")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	h := &harness{o: o}
	defer h.stop()

	result, err := h.run(ctx)
	if err != nil {
		return fmt.Errorf("%w (logs in %s)", err, o.Work)
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}

	tmp := o.Out + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("%s %s size=%d c=%d rep=%d: %.1f MiB/s racer+origin %.3f cpu-s/GiB\n",
		o.Variant, o.Mode, o.ObjectSize, o.Concurrency, o.Rep, result.MiBPerSecond, result.CPUPerGiB["racer+origin"])

	return os.Rename(tmp, o.Out)
}

type harness struct {
	o       runOptions
	procs   []*exec.Cmd
	closers []io.Closer
}

func (h *harness) logFile(name string) (*os.File, error) {
	f, err := os.Create(filepath.Join(h.o.Work, name))
	if err == nil {
		h.closers = append(h.closers, f)
	}

	return f, err
}

func (h *harness) start(name string, cmd *exec.Cmd) error {
	if cmd.Stderr == nil {
		log, err := h.logFile(name + ".log")
		if err != nil {
			return err
		}

		cmd.Stderr = log
		if cmd.Stdout == nil {
			cmd.Stdout = log
		}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}

	h.procs = append(h.procs, cmd)

	return nil
}

// stop closes the control's stdin and the logs, then terminates processes in
// reverse start order. Exit statuses after SIGTERM are expected and ignored.
func (h *harness) stop() {
	for _, c := range h.closers {
		if err := c.Close(); !errors.Is(err, os.ErrClosed) {
			warn("close", err)
		}
	}

	for i := len(h.procs) - 1; i >= 0; i-- {
		cmd := h.procs[i]
		warn("signal", cmd.Process.Signal(syscall.SIGTERM))

		done := make(chan struct{})

		go func() {
			var exit *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exit) {
				warn("wait "+cmd.Path, err)
			}

			close(done)
		}()

		select {
		case <-done:
		case <-time.After(15 * time.Second):
			warn("kill", cmd.Process.Kill())

			<-done
		}
	}
}

func freePort() (string, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { warn("close listener", l.Close()) }()

	return l.Addr().String(), nil
}

func waitFor(ctx context.Context, timeout time.Duration, what string, ok func() bool) error {
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for %s", what)
		}

		time.Sleep(20 * time.Millisecond)
	}

	return nil
}

func (h *harness) run(ctx context.Context) (*Result, error) {
	o := h.o
	if err := os.MkdirAll(filepath.Join(o.Work, "slabs"), 0o700); err != nil {
		return nil, err
	}

	for _, d := range []string{"secrets", "identity"} {
		if err := os.MkdirAll(filepath.Join(o.Work, d), 0o700); err != nil {
			return nil, err
		}
	}

	// Fake Azure Blob server.
	fake := exec.Command("taskset", "-c", o.CPUsFake, o.Fake, "-size", strconv.FormatInt(o.ObjectSize, 10))

	fakeOut, err := fake.StdoutPipe()
	if err != nil {
		return nil, err
	}

	fakeLog, err := h.logFile("fake.log")
	if err != nil {
		return nil, err
	}

	fake.Stderr = fakeLog
	if err := h.start("fake", fake); err != nil {
		return nil, err
	}

	fakeScanner := bufio.NewScanner(fakeOut)
	if !fakeScanner.Scan() {
		return nil, errors.New("fake server did not print its address")
	}

	fakeAddr := fakeScanner.Text()
	containerURL := "http://" + fakeAddr + "/" + account + "/" + container

	// Fake control plane.
	control := exec.Command(o.Control, o.Work)
	control.Env = append(os.Environ(), "RACER_BENCH_MODE="+o.Mode)

	if o.Variant == "after" {
		control.Env = append(control.Env, "RACER_BENCH_AZURE_CONTAINER_URL="+containerURL)
	}

	controlIn, err := control.StdinPipe()
	if err != nil {
		return nil, err
	}

	h.closers = append(h.closers, controlIn)

	controlOut, err := control.StdoutPipe()
	if err != nil {
		return nil, err
	}

	controlLog, err := h.logFile("control.log")
	if err != nil {
		return nil, err
	}

	control.Stderr = controlLog
	if err := h.start("control", control); err != nil {
		return nil, err
	}

	endpoint, publishedMode, err := readControlHandshake(bufio.NewScanner(controlOut), o.Mode)
	if err != nil {
		return nil, err
	}

	// Racer dataplane, settings from TestRacerMixedPerformance.
	diagnostics, err := freePort()
	if err != nil {
		return nil, err
	}

	peer, err := freePort()
	if err != nil {
		return nil, err
	}

	settings := map[string]string{
		"CLUSTER_ID": clusterID, "CONTROL_ENDPOINT": endpoint,
		"PEER_LISTEN": peer, "DIAGNOSTICS_LISTEN": diagnostics, "MAX_THREADS": strconv.Itoa(o.MaxThreads), "ENABLE_RDMA": "false",
		"PLAINTEXT_BYTES": "268435456", "CIPHERTEXT_BYTES": "268435456", "DIRTY_BYTES": "134217728", "REGISTERED_BYTES": "1",
		"REQUEST_CONTEXT_BYTES": "16777216", "SLAB_BYTES": strconv.FormatInt(o.SlabBytes, 10), "SEGMENT_BYTES": "67108864", "FREE_SEGMENT_RESERVE": "2",
		"QUEUE_ENTRIES": "256", "CLIENT_CONNECTIONS": "128", "ORIGIN_CONNECTIONS_PER_CACHE": "8", "METADATA_ENTRIES": "128", "FLIGHTS": "64", "PIPES": "16", "RANGE_WINDOW_PAGES": "2",
		"REQUEST_TIMEOUT_MS": "30000", "READER_STALL_TIMEOUT_MS": "10000", "SHUTDOWN_TIMEOUT_MS": "10000",
	}
	for name, path := range map[string]string{"TRUST_BUNDLE": "trust.pem", "SERVICE_ACCOUNT_TOKEN": "token", "SECRET_DIRECTORY": "secrets", "IDENTITY_DIRECTORY": "identity", "SLAB_DIRECTORY": "slabs"} {
		settings[name] = filepath.Join(o.Work, path)
	}
	// RACER_BENCH_RACER_<NAME>=value overrides any racer setting.
	for _, env := range os.Environ() {
		if rest, ok := strings.CutPrefix(env, "RACER_BENCH_RACER_"); ok {
			if k, v, ok := strings.Cut(rest, "="); ok {
				settings[k] = v
			}
		}
	}

	racer := exec.Command("taskset", "-c", o.CPUsRacer, o.Racer)

	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "RACER_") {
			racer.Env = append(racer.Env, env)
		}
	}

	for k, v := range settings {
		racer.Env = append(racer.Env, "RACER_"+k+"="+v)
	}

	if data, err := json.MarshalIndent(settings, "", "  "); err == nil {
		warn("write config", os.WriteFile(filepath.Join(o.Work, "dataplane-config.json"), data, 0o644))
	}

	if err := h.start("racer", racer); err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: time.Second}
	get := func(url string) ([]byte, bool) {
		resp, err := httpClient.Get(url)
		if err != nil {
			return nil, false
		}
		defer func() { warn("close body", resp.Body.Close()) }()

		data, err := io.ReadAll(resp.Body)

		return data, err == nil && resp.StatusCode == http.StatusOK
	}

	if err := waitFor(ctx, o.ReadyTimeout, "racer /readyz", func() bool { _, ok := get("http://" + diagnostics + "/readyz"); return ok }); err != nil {
		return nil, err
	}

	pids := map[string]int{"racer": racer.Process.Pid, "fake": fake.Process.Pid, "loadgen": os.Getpid()}

	if o.Variant == "before" {
		originDir := "/run/racer/" + cacheName + "/origin"
		if err := waitFor(ctx, o.ReadyTimeout, originDir, func() bool {
			info, err := os.Stat(originDir)
			if err != nil {
				return false
			}

			stat, ok := info.Sys().(*syscall.Stat_t)

			return ok && stat.Uid == originUID
		}); err != nil {
			return nil, err
		}

		args := []string{"-c", o.CPUsOrigin, "setpriv", "--reuid", strconv.Itoa(originUID), "--regid", strconv.Itoa(originUID)}
		if o.OriginGroups != "" {
			args = append(args, "--groups", o.OriginGroups)
		} else {
			args = append(args, "--clear-groups")
		}

		args = append(args, "--", o.RacerObject, "origin", "--provider", "azure", "--container-url", containerURL,
			"--azure-credential", "anonymous", "--cache", cacheName, "--cache-uid", cacheUID)
		if o.OriginMaxRequests > 0 {
			args = append(args, "--max-concurrent-requests", strconv.Itoa(o.OriginMaxRequests))
		}

		args = append(args, strings.Fields(o.OriginExtraArgs)...)

		origin := exec.Command("taskset", args...)
		if err := h.start("origin", origin); err != nil {
			return nil, err
		}

		if err := waitFor(ctx, o.ReadyTimeout, "origin socket", func() bool { _, err := os.Stat(originDir + "/socket"); return err == nil }); err != nil {
			return nil, err
		}

		pids["origin"] = origin.Process.Pid
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cacheName, MaxConnections: o.Concurrency, PageCredits: o.PageCredits})
	if err != nil {
		return nil, err
	}
	defer func() { warn("close client", client.Close()) }()

	// Untimed probe: one tiny object proves the path end to end.
	if _, err := readObject(ctx, client, "size-1/probe"); err != nil {
		return nil, fmt.Errorf("probe read: %w", err)
	}

	objects := int(o.TotalBytes / o.ObjectSize)
	before := map[string]CPU{}

	fakeBefore, _ := fakeStats(get, fakeAddr)

	for name, pid := range pids {
		before[name] = readCPU(pid)
	}

	started := time.Now()

	var (
		next     atomic.Int64
		total    atomic.Int64
		wg       sync.WaitGroup
		firstErr error
		errOnce  sync.Once
		retries  atomic.Int64
		wasted   atomic.Int64
		retryLog atomic.Value
	)

	for range o.Concurrency {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= objects {
					return
				}

				key := fmt.Sprintf("obj/%06d", i)

				n, err := readObject(ctx, client, key)
				// Retry transient failures (for example the Go origin's
				// four-slot metadata limit) on the whole object, as clients do.
				for attempt := 1; err != nil && !errors.Is(err, errMismatch) && attempt <= maxRetries && ctx.Err() == nil; attempt++ {
					retries.Add(1)
					wasted.Add(n)
					retryLog.Store(err.Error())
					time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)

					n, err = readObject(ctx, client, key)
				}

				if err == nil && n != o.ObjectSize {
					err = fmt.Errorf("object %d: read %d bytes, want %d", i, n, o.ObjectSize)
				}

				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}

				total.Add(n)
			}
		})
	}

	wg.Wait()

	if firstErr != nil {
		for name, path := range map[string]string{"failure-racer.prom": "/metrics", "failure-terminal.txt": "/debug/terminal"} {
			if data, ok := get("http://" + diagnostics + path); ok {
				warn("write "+name, os.WriteFile(filepath.Join(o.Work, name), data, 0o644))
			}
		}

		return nil, firstErr
	}

	readSeconds := time.Since(started).Seconds()

	// Include background fill and disk write completion in the window.
	drainErr := waitFor(ctx, 30*time.Second, "racer drain", func() bool {
		m, ok := get("http://" + diagnostics + "/metrics")
		s := string(m)

		return ok && strings.Contains(s, "\nracer_active_fills 0\n") && strings.Contains(s, "\nracer_pending_disk_writes 0\n")
	})

	warn("drain", drainErr)

	result := &Result{
		Variant: o.Variant, Mode: o.Mode, PublishedMode: publishedMode, Rep: o.Rep, ObjectSize: o.ObjectSize, Concurrency: o.Concurrency,
		Objects: objects, Bytes: total.Load(), Seconds: readSeconds, CPU: map[string]CPU{}, CPUPerGiB: map[string]float64{},
		CPUSets:  map[string]string{"racer": o.CPUsRacer, "origin": o.CPUsOrigin, "fake": o.CPUsFake},
		Binaries: map[string]string{"racer": o.Racer, "control": o.Control, "racer-object": o.RacerObject, "fake": o.Fake},
		Started:  started, ClockTicks: clockTicks, Retries: retries.Load(), RetriedBytes: wasted.Load(),
	}

	if v, ok := retryLog.Load().(string); ok {
		result.LastRetryError = v
	}

	for name, pid := range pids {
		after := readCPU(pid)
		b := before[name]
		result.CPU[name] = CPU{
			UserSeconds: after.UserSeconds - b.UserSeconds, SystemSeconds: after.SystemSeconds - b.SystemSeconds,
			Seconds: after.Seconds - b.Seconds, SchedstatSecond: after.SchedstatSecond - b.SchedstatSecond, Threads: after.Threads,
		}
	}

	if fakeAfter, ok := fakeStats(get, fakeAddr); ok {
		result.Fake = fakeblob.Snapshot{
			Heads: fakeAfter.Heads - fakeBefore.Heads, Gets: fakeAfter.Gets - fakeBefore.Gets,
			Errors: fakeAfter.Errors - fakeBefore.Errors, Bytes: fakeAfter.Bytes - fakeBefore.Bytes,
		}
	}

	gib := float64(result.Bytes) / (1 << 30)
	result.MiBPerSecond = float64(result.Bytes) / (1 << 20) / readSeconds

	for name, cpu := range result.CPU {
		result.CPUPerGiB[name] = cpu.Seconds / gib
	}

	result.CPUPerGiB["racer+origin"] = (result.CPU["racer"].Seconds + result.CPU["origin"].Seconds) / gib

	if m, ok := get("http://" + diagnostics + "/metrics"); ok {
		warn("write metrics", os.WriteFile(filepath.Join(o.Work, "final-racer.prom"), m, 0o644))
	}

	return result, nil
}

// readControlHandshake reads the control's endpoint line and its
// "mode=<mode>" line, and fails unless the published mode equals want. A
// control that ignores RACER_BENCH_MODE does not print the mode line, so it
// is rejected rather than silently benchmarking a different cache mode.
func readControlHandshake(scanner *bufio.Scanner, want string) (endpoint, mode string, err error) {
	if !scanner.Scan() {
		return "", "", errors.New("control did not print its endpoint")
	}

	endpoint = scanner.Text()

	if !scanner.Scan() {
		return "", "", errors.New("control did not report its published mode; use a mode-aware racer-performance-control")
	}

	mode, ok := strings.CutPrefix(scanner.Text(), "mode=")
	if !ok {
		return "", "", fmt.Errorf("control printed %q, want mode=<mode>; use a mode-aware racer-performance-control", scanner.Text())
	}

	if mode != want {
		return "", "", fmt.Errorf("control published mode %q, requested %q", mode, want)
	}

	return endpoint, mode, nil
}

func fakeStats(get func(string) ([]byte, bool), addr string) (fakeblob.Snapshot, bool) {
	var s fakeblob.Snapshot

	data, ok := get("http://" + addr + fakeblob.StatsPath)
	if !ok || json.Unmarshal(data, &s) != nil {
		return s, false
	}

	return s, true
}

func readObject(ctx context.Context, client *racersdk.Client, key string) (int64, error) {
	request, err := racerobject.NewRequest(container, key, "")
	if err != nil {
		return 0, err
	}

	value, err := client.Get(ctx, request)
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { warn("close object", value.Close()) }()

	verifier := fakeblob.NewVerifier(key)
	if _, err := value.WriteTo(verifier); err != nil {
		if verifier.Mismatch() {
			return verifier.Bytes(), fmt.Errorf("read %s: %w: %w", key, errMismatch, err)
		}

		return verifier.Bytes(), fmt.Errorf("read %s: %w", key, err)
	}

	return verifier.Bytes(), nil
}

// warn reports a non-fatal error; diagnostics must not fail a measurement.
func warn(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "racer-origin-bench: %s: %v\n", what, err)
	}
}

const maxRetries = 50

var errMismatch = errors.New("payload mismatch")

// clockTicks is USER_HZ; Linux reports 100 on every supported architecture.
var clockTicks = 100

func readCPU(pid int) CPU {
	var c CPU

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return c
	}

	s := string(data)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])

	if len(fields) > 12 {
		u, uerr := strconv.ParseFloat(fields[11], 64)
		st, serr := strconv.ParseFloat(fields[12], 64)
		warn("parse stat", errors.Join(uerr, serr))

		c.UserSeconds, c.SystemSeconds = u/float64(clockTicks), st/float64(clockTicks)
		c.Seconds = c.UserSeconds + c.SystemSeconds
	}

	tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/schedstat", pid))
	warn("glob tasks", err)

	c.Threads = len(tasks)

	for _, task := range tasks {
		if data, err := os.ReadFile(task); err == nil {
			if f := strings.Fields(string(data)); len(f) > 0 {
				ns, err := strconv.ParseFloat(f[0], 64)
				warn("parse schedstat", err)

				c.SchedstatSecond += ns / 1e9
			}
		}
	}

	return c
}

func summarizeCommand(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory of result JSON files")

	if err := fs.Parse(args); err != nil {
		return err
	}

	files, err := filepath.Glob(filepath.Join(*dir, "*.json"))
	if err != nil {
		return err
	}

	type key struct {
		mode        string
		size        int64
		concurrency int
	}

	groups := map[key]map[string][]Result{}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}

		var r Result
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}

		k := key{r.Mode, r.ObjectSize, r.Concurrency}
		if groups[k] == nil {
			groups[k] = map[string][]Result{}
		}

		groups[k][r.Variant] = append(groups[k][r.Variant], r)
	}

	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.mode != b.mode {
			return a.mode < b.mode
		}

		if a.size != b.size {
			return a.size < b.size
		}

		return a.concurrency < b.concurrency
	})

	fmt.Println("CPU seconds (utime+stime) per GiB read, median across repetitions. Cold reads of distinct keys through racersdk.")
	fmt.Println()
	fmt.Println("| mode | object | C | variant | reps | racer+origin | racer | origin | fake | loadgen | MiB/s | retries |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|---|")

	median := func(rs []Result, f func(Result) float64) float64 {
		v := make([]float64, 0, len(rs))
		for _, r := range rs {
			v = append(v, f(r))
		}

		sort.Float64s(v)

		if len(v) == 0 {
			return math.NaN()
		}

		if len(v)%2 == 1 {
			return v[len(v)/2]
		}

		return (v[len(v)/2-1] + v[len(v)/2]) / 2
	}

	var deltas []string

	for _, k := range keys {
		head := map[string]float64{}

		for _, variant := range []string{"before", "after"} {
			rs := groups[k][variant]
			if len(rs) == 0 {
				continue
			}

			per := func(name string) float64 { return median(rs, func(r Result) float64 { return r.CPUPerGiB[name] }) }
			head[variant] = per("racer+origin")
			fmt.Printf("| %s | %d MiB | %d | %s | %d | **%.3f** | %.3f | %.3f | %.3f | %.3f | %.0f | %.0f |\n",
				k.mode, k.size>>20, k.concurrency, variant, len(rs), head[variant], per("racer"), per("origin"),
				per("fake"), per("loadgen"), median(rs, func(r Result) float64 { return r.MiBPerSecond }),
				median(rs, func(r Result) float64 { return float64(r.Retries) }))
		}

		if b, a := head["before"], head["after"]; b > 0 && a > 0 {
			deltas = append(deltas, fmt.Sprintf("| %s | %d MiB | %d | %.3f | %.3f | %+.1f%% |", k.mode, k.size>>20, k.concurrency, b, a, (a-b)/b*100))
		}
	}

	if len(deltas) > 0 {
		fmt.Println()
		fmt.Println("| mode | object | C | before racer+origin | after racer | change |")
		fmt.Println("|---|---|---|---|---|---|")

		for _, d := range deltas {
			fmt.Println(d)
		}
	}

	return nil
}
