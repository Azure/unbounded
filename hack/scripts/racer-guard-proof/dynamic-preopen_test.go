package main

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

func TestCaptureOutput(t *testing.T) {
	row := captureRow{Node: "n"}
	armed := make(chan string, 1)
	w := captureOutput{row: &row, armed: armed}
	for _, part := range []string{`{"event":"armed","node":"n"}`, "\n", `{"event":"proof","node":"n","proof":{"node":"n"},"fetchSeconds":0.1}`, "\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if <-armed != "n" || row.Proof == nil || row.Received.IsZero() || row.FetchSeconds != 0.1 {
		t.Fatal(row)
	}
	if _, err := w.Write([]byte("{\"event\":\"proof\",\"node\":\"n\",\"proof\":{\"node\":\"n\"}}\n")); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestCaptureRejects(t *testing.T) {
	for _, s := range []string{"not json\n", "{\"event\":\"armed\",\"node\":\"other\"}\n", "{\"event\":\"proof\",\"node\":\"n\",\"proof\":{\"node\":\"n\"}}\n", strings.Repeat("x", 16385)} {
		row := captureRow{Node: "n"}
		w := captureOutput{row: &row, armed: make(chan string, 1)}
		if _, err := w.Write([]byte(s)); err == nil {
			t.Fatal("invalid stream accepted")
		}
	}
}

func TestRemoteBudget(t *testing.T) {
	now := time.Now()
	cmd := remoteCommand(now.Add(30*time.Second), now)
	if cmd[3] != "35.000s" || !strings.Contains(remoteCapture, "signal.alarm(5)") || !strings.Contains(remoteCapture, "ready.active(host,policy,node)") {
		t.Fatal(cmd)
	}
	if strings.Contains(remoteCapture, "START") || strings.Contains(remoteCapture, "reconcile(") {
		t.Fatal("mutation in capture")
	}
}

func TestResidentBudgetAndErrorOutput(t *testing.T) {
	rss, err := residentBytes()
	if err != nil || rss == 0 {
		t.Fatalf("resident bytes %d: %v", rss, err)
	}
	w := &cappedError{}
	if _, err := w.Write([]byte(strings.Repeat("x", 8192))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("stderr overflow accepted")
	}
}

// Exercise the production launch loop without Kubernetes or remote processes.
func TestCapturePipes(t *testing.T) {
	for _, size := range []int{0, 1, 1500} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			pods := make([]v1.Pod, size)
			for i := range pods {
				pods[i].Spec.NodeName = strconv.Itoa(i)
			}
			type stream struct {
				node     string
				released <-chan struct{}
			}
			started := make(chan stream, size)
			finished := make(chan struct{}, size)
			writers, releases := startCaptureStreams(pods, func(p v1.Pod, r *io.PipeReader, released <-chan struct{}) {
				defer func() { _ = r.Close(); finished <- struct{}{} }()
				started <- stream{p.Spec.NodeName, released}
				<-released
				data, err := io.ReadAll(r)
				if err != nil || string(data) != p.Spec.NodeName {
					t.Errorf("node %s: data=%q err=%v", p.Spec.NodeName, data, err)
				}
			})
			if len(writers) != size || len(releases) != size {
				t.Fatal("missing stream handles")
			}
			seen := make(map[string]bool, size)
			for range size {
				s := <-started
				if s.released == nil || s.released != releases[s.node] || seen[s.node] {
					t.Fatal("wrong or duplicate worker snapshot")
				}
				seen[s.node] = true
				close(releases[s.node])
				if _, err := io.WriteString(writers[s.node], s.node); err != nil {
					t.Fatal(err)
				}
				_ = writers[s.node].Close()
			}
			for range size {
				<-finished
			}
		})
	}
}

func TestCapturePipesClosedBeforeGO(t *testing.T) {
	finished := make(chan error, 1)
	pods := []v1.Pod{{Spec: v1.PodSpec{NodeName: "n"}}}
	writers, _ := startCaptureStreams(pods, func(_ v1.Pod, r *io.PipeReader, _ <-chan struct{}) {
		defer r.Close()
		_, err := io.ReadAll(r)
		finished <- err
	})
	_ = writers["n"].CloseWithError(io.ErrClosedPipe)
	if err := <-finished; err != io.ErrClosedPipe {
		t.Fatalf("canceled stream: %v", err)
	}
}
