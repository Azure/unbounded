// Operational collection only: no cluster writes, retries, START, or policy changes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// The existing ready.active performs all socket privacy, root peer, host-netns,
// boot, identity, expiry and age checks. Neither it nor the server is modified.
const remoteCapture = `import sys,os,json,time,signal
sys.path.insert(0,'/guard')
import ready
from pathlib import Path
target=float(sys.argv[1])
policy=json.loads(Path('/policy/policy.json').read_text())
host=ready.local.Host(os.environ['NODE_IP'],policy['monitors'])
node=os.environ['NODE_NAME']
print(json.dumps(dict(event='armed',node=node,remoteArmed=time.time())),flush=True)
assert sys.stdin.readline().strip()=='GO', 'capture not committed'
assert time.time()<target, 'missed target'
while time.time()<target:
    time.sleep(min(0.05,max(0,target-time.time())))
signal.signal(signal.SIGALRM,ready.w.expired)
signal.alarm(5)
begin=time.time()
mono=time.monotonic()
proof=ready.active(host,policy,node)
end=time.time()
print(json.dumps(dict(event='proof',node=node,proof=proof,remoteBegin=begin,remoteEnd=end,fetchSeconds=time.monotonic()-mono)),flush=True)
signal.alarm(0)
`

type captureRow struct {
	Pod          string         `json:"pod"`
	UID          string         `json:"uid"`
	Node         string         `json:"node"`
	Armed        time.Time      `json:"armed"`
	Received     time.Time      `json:"received"`
	Exited       time.Time      `json:"exited"`
	RemoteBegin  float64        `json:"remoteBegin"`
	RemoteEnd    float64        `json:"remoteEnd"`
	FetchSeconds float64        `json:"fetchSeconds"`
	Proof        map[string]any `json:"proof,omitempty"`
	Error        string         `json:"error,omitempty"`
	Stderr       string         `json:"stderr,omitempty"`
}

// Each stdout stream has a fixed byte budget. Parsing happens on arrival, not
// after exec exit; admission still requires BOTH a proof and a successful exit.
type captureOutput struct {
	row    *captureRow
	armed  chan<- string
	buffer []byte
	total  int
}

func (w *captureOutput) Write(p []byte) (int, error) {
	w.total += len(p)
	if w.total > 16384 {
		return 0, errors.New("stdout budget exceeded")
	}
	w.buffer = append(w.buffer, p...)
	for {
		i := bytes.IndexByte(w.buffer, '\n')
		if i < 0 {
			break
		}
		var event struct {
			Event        string         `json:"event"`
			Node         string         `json:"node"`
			Proof        map[string]any `json:"proof"`
			RemoteBegin  float64        `json:"remoteBegin"`
			RemoteEnd    float64        `json:"remoteEnd"`
			FetchSeconds float64        `json:"fetchSeconds"`
		}
		if err := json.Unmarshal(w.buffer[:i], &event); err != nil {
			return 0, err
		}
		w.buffer = w.buffer[i+1:]
		if event.Node != w.row.Node {
			return 0, errors.New("stream node mismatch")
		}
		switch event.Event {
		case "armed":
			if !w.row.Armed.IsZero() {
				return 0, errors.New("duplicate armed")
			}
			w.row.Armed = time.Now().UTC()
			w.armed <- event.Node
		case "proof":
			if w.row.Armed.IsZero() || w.row.Proof != nil || event.Proof["node"] != w.row.Node {
				return 0, errors.New("invalid proof event")
			}
			w.row.Received = time.Now().UTC()
			w.row.Proof = event.Proof
			w.row.RemoteBegin = event.RemoteBegin
			w.row.RemoteEnd = event.RemoteEnd
			w.row.FetchSeconds = event.FetchSeconds
		default:
			return 0, errors.New("unknown stream event")
		}
	}
	return len(p), nil
}

type cappedError struct{ bytes.Buffer }

func (w *cappedError) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 8192 {
		return 0, errors.New("stderr budget exceeded")
	}
	return w.Buffer.Write(p)
}

func remoteCommand(target time.Time, now time.Time) []string {
	// Operational process allowance only. All security TTLs remain in ready/server.
	seconds := target.Sub(now).Seconds() + 5
	return []string{"timeout", "--signal=TERM", "--kill-after=10s", fmt.Sprintf("%.3fs", seconds), "python3", "-u", "-B", "-c", remoteCapture, fmt.Sprintf("%.6f", float64(target.UnixMicro())/1e6)}
}

func residentBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, errors.New("invalid statm")
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	return pages * uint64(os.Getpagesize()), err
}

// startCaptureStreams keeps both maps on the caller goroutine. Each worker gets
// its own handles before launch, never a lookup while setup is mutating the maps.
func startCaptureStreams(pods []v1.Pod, run func(v1.Pod, *io.PipeReader, <-chan struct{})) (map[string]*io.PipeWriter, map[string]chan struct{}) {
	writers := map[string]*io.PipeWriter{}
	releases := map[string]chan struct{}{}
	for _, p := range pods {
		released := make(chan struct{})
		releases[p.Spec.NodeName] = released
		r, w := io.Pipe()
		writers[p.Spec.NodeName] = w
		go run(p, r, released)
	}
	return writers, releases
}

func main() {
	if err := runCapture(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCapture() error {
	podNames := flag.String("pods", "", "exactly three comma-separated canary pods; mutually exclusive with -fleet")
	fleet := flag.Bool("fleet", false, "explicit full-fleet authorization; do not use before qualification")
	output := flag.String("output", "preopen-canary.json", "exclusive evidence file")
	lead := flag.Duration("lead", 30*time.Second, "pre-establishment allowance, 10s..75s")
	openers := flag.Int("openers", 64, "max concurrent stream establishments, 1..64")
	flag.Parse()
	if *lead < 10*time.Second || *lead > 75*time.Second || *openers < 1 || *openers > 64 || (*fleet == (*podNames != "")) {
		return errors.New("invalid capture flags")
	}
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	journal, err := os.OpenFile(*output+".jsonl", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer journal.Close()
	var logMu sync.Mutex
	emit := func(event string, details any) {
		logMu.Lock()
		defer logMu.Unlock()
		row := map[string]any{"at": time.Now().UTC(), "event": event, "details": details}
		_ = json.NewEncoder(journal).Encode(row)
		if event != "row" {
			_ = json.NewEncoder(os.Stdout).Encode(row)
		}
	}
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, 110*time.Second)
	defer cancel()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: "joolshev-scale-test"}).ClientConfig()
	if err != nil {
		return err
	}
	cfg.QPS, cfg.Burst = 64, 64
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	emit("inventory-start", nil)
	pl, err := client.CoreV1().Pods("unbounded-system").List(ctx, metav1.ListOptions{LabelSelector: "app=racer-stage47-guard"})
	if err != nil {
		return err
	}
	if len(pl.Items) != 1500 {
		return errors.New("expected 1500 guard pods")
	}
	wanted := map[string]bool{}
	if !*fleet {
		for _, p := range strings.Split(*podNames, ",") {
			if p == "" || wanted[p] {
				return errors.New("duplicate/empty canary")
			}
			wanted[p] = true
		}
		if len(wanted) != 3 {
			return errors.New("exactly three canaries required")
		}
	}
	var pods []v1.Pod
	seen := map[string]bool{}
	for _, p := range pl.Items {
		if p.DeletionTimestamp != nil || p.Spec.NodeName == "" || seen[p.Spec.NodeName] {
			return errors.New("inventory drift")
		}
		seen[p.Spec.NodeName] = true
		if *fleet || wanted[p.Name] {
			pods = append(pods, p)
		}
	}
	if (!*fleet && len(pods) != 3) || (*fleet && len(pods) != 1500) {
		return errors.New("selected pod missing")
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	nl, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, p := range pods {
		selected[p.Spec.NodeName] = true
	}
	nodes := []v1.Node{}
	for _, n := range nl.Items {
		if selected[n.Name] {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) != len(pods) {
		return errors.New("node inventory mismatch")
	}
	target := time.Now().Add(*lead).UTC()
	deadline := target.Add(-2 * time.Second)
	collect, finish := context.WithDeadline(ctx, target.Add(8*time.Second))
	defer finish()
	armed := make(chan string, len(pods))
	done := make(chan captureRow, len(pods))
	slots := make(chan struct{}, *openers)
	writers, releases := startCaptureStreams(pods, func(p v1.Pod, reader *io.PipeReader, released <-chan struct{}) {
		defer reader.Close()
		row := captureRow{Pod: p.Name, UID: string(p.UID), Node: p.Spec.NodeName}
		select {
		case slots <- struct{}{}:
		case <-collect.Done():
			row.Error = collect.Err().Error()
			done <- row
			return
		}
		// The opener permit is released on ARMED, while the stream remains live.
		go func() {
			select {
			case <-released:
			case <-collect.Done():
			}
			<-slots
		}()
		req := client.CoreV1().RESTClient().Post().Resource("pods").Namespace("unbounded-system").Name(p.Name).SubResource("exec").VersionedParams(&v1.PodExecOptions{Container: p.Spec.Containers[0].Name, Command: remoteCommand(target, time.Now()), Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		executor, e := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, req.URL())
		stdout := &captureOutput{row: &row, armed: armed}
		stderr := &cappedError{}
		if e == nil {
			e = executor.StreamWithContext(collect, remotecommand.StreamOptions{Stdin: reader, Stdout: stdout, Stderr: stderr})
		}
		row.Exited = time.Now().UTC()
		row.Stderr = stderr.String()
		if e != nil {
			row.Error = e.Error()
		} else if row.Proof == nil || len(stdout.buffer) != 0 {
			row.Error = "missing/incomplete proof"
		}
		done <- row
	})
	emit("preopen-start", map[string]any{"target": target, "streams": len(pods), "openers": *openers, "armDeadline": deadline})
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	budget := time.NewTicker(time.Second)
	defer budget.Stop()
	rows := []captureRow{}
	count := 0
	committed := false
	failed := ""
	for len(rows) < len(pods) {
		select {
		case node := <-armed:
			close(releases[node])
			count++
			if count == len(pods) && time.Now().Before(deadline) && failed == "" {
				committed = true
				emit("all-armed", count)
				for _, w := range writers {
					go func(w *io.PipeWriter) { _, _ = io.WriteString(w, "GO\n"); _ = w.Close() }(w)
				}
			}
		case row := <-done:
			rows = append(rows, row)
			emit("row", row)
			if row.Error != "" && failed == "" {
				failed = row.Error
				finish()
			}
		case <-timer.C:
			if !committed {
				failed = "all-streams-armed deadline missed"
				finish()
			}
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			rss, _ := residentBytes()
			emit("heartbeat", map[string]any{"armed": count, "completed": len(rows), "heapBytes": m.HeapAlloc, "residentBytes": rss, "goroutines": runtime.NumGoroutine()})
		case <-budget.C:
			rss, e := residentBytes()
			if e != nil || rss > 768*1024*1024 {
				failed = "local RSS budget exceeded or unreadable"
				finish()
			}
		case <-collect.Done():
			for _, w := range writers {
				_ = w.Close()
			}
			// Drain once, avoiding a busy loop on the canceled context.
			for len(rows) < len(pods) {
				row := <-done
				rows = append(rows, row)
				emit("row", row)
			}
		}
	}
	proofs := []map[string]any{}
	for _, r := range rows {
		if r.Error == "" && r.Proof != nil {
			proofs = append(proofs, r.Proof)
		}
	}
	result := map[string]any{"target": target, "rows": rows, "nodes": nodes, "proofs": proofs, "successful": len(proofs), "expected": len(pods), "committed": committed, "fleet": *fleet, "error": failed, "requiresCurrentTimeVerification": true}
	if committed && len(proofs) == len(pods) {
		cm, e := client.CoreV1().ConfigMaps("unbounded-system").Get(ctx, "racer-stage47-sources", metav1.GetOptions{})
		if e != nil {
			failed = e.Error()
			result["error"] = failed
		} else {
			result["cm"] = cm
		}
	}
	result["savedAt"] = time.Now().UTC()
	if err = json.NewEncoder(file).Encode(result); err != nil {
		return err
	}
	emit("saved", map[string]any{"successful": len(proofs), "expected": len(pods), "error": failed})
	if failed != "" || !committed || len(proofs) != len(pods) {
		return errors.New("capture failed; partial evidence retained")
	}
	return nil
}
