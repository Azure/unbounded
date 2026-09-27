//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const peerPageSize = 16 << 20

type peerNode struct {
	name, uid, pod, ip string
}

func (h *harness) peerNodes() [2]peerNode {
	h.t.Helper()

	var pods struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct{ NodeName string }
			Status   struct{ PodIP string }
		}
	}

	require.NoError(h.t, json.Unmarshal([]byte(h.kubectl("get", "pods", "-n", namespace, "-l", "app.kubernetes.io/name=racer-dataplane", "-o", "json")), &pods))
	require.Len(h.t, pods.Items, 2, "peer test requires exactly two deployed dataplanes")

	var nodes [2]peerNode
	for i, pod := range pods.Items {
		nodes[i] = peerNode{name: pod.Spec.NodeName, pod: pod.Metadata.Name, ip: pod.Status.PodIP}
		nodes[i].uid = strings.TrimSpace(h.kubectl("get", "node", nodes[i].name, "-o", "jsonpath={.metadata.uid}"))
		require.NotEmpty(h.t, nodes[i].ip)
		require.NotEmpty(h.t, nodes[i].uid)
	}

	require.NotEqual(h.t, nodes[0].name, nodes[1].name)

	return nodes
}

type peerOrigin struct {
	body  []byte
	id    string
	mu    sync.Mutex
	gets  map[string]int
	heads int
}

func (h *harness) newPeerFixture(nodes [2]peerNode) *peerOrigin {
	h.t.Helper()
	cache := strings.TrimSpace(h.kubectl("get", "clustercache", "gantry", "-o", "jsonpath={.metadata.uid}"))
	require.NotEmpty(h.t, cache)

	fixture := &peerOrigin{body: make([]byte, 2*peerPageSize+113), gets: make(map[string]int)}
	random := rand.NewChaCha8([32]byte{81})
	_, err := random.Read(fixture.body)
	require.NoError(h.t, err)
	// Equal default shares rank by quantized HRW cost and then node ID. Select an
	// immutable object whose three pages belong first to the serving node, so
	// each cold reader page must probe that node before it can fetch origin.
	for nonce := uint64(0); nonce < 256; nonce++ {
		binary.BigEndian.PutUint64(fixture.body, nonce)

		key := sha256.Sum256(fixture.body)
		if peerRanksFirst(cache, key, 0, nodes) && peerRanksFirst(cache, key, 1, nodes) && peerRanksFirst(cache, key, 2, nodes) {
			fixture.id = "sha256:" + hex.EncodeToString(key[:])
			h.t.Logf("peer fixture %s: serving=%s reading=%s", fixture.id, nodes[0].name, nodes[1].name)

			return fixture
		}
	}

	h.t.Fatal("could not select a three-page fixture with the serving node ranked first")

	return nil
}

// This is only fixture selection, not a substitute for peer-hit assertions.
// The canonical encodings are topology/hash.rs and topology/placement.rs.
func peerRanksFirst(cache string, key [32]byte, page uint64, nodes [2]peerNode) bool {
	input := []byte("racer/slot/v1\x00")
	input = binary.BigEndian.AppendUint32(input, uint32(len(cache)))
	input = append(input, cache...)
	input = append(input, key[:]...)
	input = binary.BigEndian.AppendUint64(input, page)
	slotHash := sha256.Sum256(input)
	slot := binary.BigEndian.Uint32(slotHash[:4]) >> 12

	var scores [2]uint64

	for i, node := range nodes {
		input = []byte("racer/hrw/v1\x00")
		input = binary.BigEndian.AppendUint32(input, slot)
		input = binary.BigEndian.AppendUint32(input, uint32(len(node.uid)))
		input = append(input, node.uid...)
		score := sha256.Sum256(input)
		scores[i] = binary.BigEndian.Uint64(score[:8])
	}

	return peerScoreFirst(scores, [2]string{nodes[0].uid, nodes[1].uid})
}

func peerScoreFirst(samples [2]uint64, nodes [2]string) bool {
	left, right := peerExponentialCost(samples[0]), peerExponentialCost(samples[1])
	return left < right || left == right && nodes[0] < nodes[1]
}

// Match placement.rs's Q32.32 integer logarithm, including quantization ties.
// Mul64 retains the same 128-bit square without floating-point approximation.
func peerExponentialCost(sample uint64) uint64 {
	if sample == ^uint64(0) {
		return 1
	}

	value := sample + 1
	exponent := bits.Len64(value) - 1
	normalized := value << (63 - exponent)

	var fraction uint64

	for bit := 31; bit >= 0; bit-- {
		hi, lo := bits.Mul64(normalized, normalized)
		if hi>>63 != 0 {
			normalized = hi
			fraction |= uint64(1) << bit
		} else {
			normalized = hi<<1 | lo>>63
		}
	}

	return (uint64(64-exponent) << 32) - fraction
}

func (o *peerOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v2/fixture/peers/blobs/"+o.id {
		http.NotFound(w, r)
		return
	}

	o.mu.Lock()
	switch r.Method {
	case http.MethodGet:
		o.gets[r.Header.Get("Range")]++
	case http.MethodHead:
		o.heads++
	}
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", o.id)
	http.ServeContent(w, r, o.id, time.Time{}, bytes.NewReader(o.body))
}

func (o *peerOrigin) counts() (map[string]int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	gets := make(map[string]int, len(o.gets))
	for key, value := range o.gets {
		gets[key] = value
	}

	return gets, o.heads
}

func (h *harness) verifyPeerCache(nodes [2]peerNode, fixture *peerOrigin) {
	h.t.Helper()

	server, reader := nodes[0], nodes[1]
	serverURL := h.forward(server.pod, "9090", "/readyz")
	readerURL := h.forward(reader.pod, "9090", "/readyz")

	beforeWarm := h.peerMetrics(serverURL, "before-warm")
	for page := 0; page < 3; page++ {
		h.readPeerPage(server, fixture, page)
	}

	afterWarm := h.peerMetrics(serverURL, "after-warm")
	require.Equal(h.t, uint64(3), afterWarm["racer_origin_fills_total"]-beforeWarm["racer_origin_fills_total"])

	warmGets, _ := fixture.counts()
	require.Equal(h.t, map[string]int{"": 1, "bytes=16777216-": 1, "bytes=33554432-": 1}, warmGets, "warm each page exactly once through Gantry's origin adapter")

	before := h.peerMetrics(readerURL, "before-peer-hit")
	h.readPeerPage(reader, fixture, 0)
	after := h.peerMetrics(readerURL, "after-peer-hit")
	require.Equal(h.t, uint64(1), after["racer_peer_hits_total"]-before["racer_peer_hits_total"], "reader must receive a validated page over the real peer transport")
	require.Equal(h.t, before["racer_origin_fills_total"], after["racer_origin_fills_total"])

	gets, _ := fixture.counts()
	require.Equal(h.t, warmGets, gets, "peer reuse must not fetch corresponding origin data, including page zero")

	// Reject only the reader's traffic to the serving pod's peer port. Keep the
	// pod, identity, membership, diagnostics, and origin adapter alive. Readiness
	// is not a membership eviction signal, and the next page is cold on reader.
	rule := []string{"FORWARD", "-s", reader.ip, "-d", server.ip, "-p", "tcp", "--dport", "7443", "-m", "comment", "--comment", "racer-e2e-peer-outage", "-j", "REJECT", "--reject-with", "tcp-reset"}
	h.run("docker", append([]string{"exec", server.name, "iptables", "-I"}, rule...)...)

	blocked := true

	h.t.Cleanup(func() {
		if blocked {
			h.command(context.Background(), "docker", append([]string{"exec", server.name, "iptables", "-D"}, rule...)...)
		}
	})

	before = after
	started := time.Now()

	h.readPeerPage(reader, fixture, 1)

	elapsed := time.Since(started)
	require.Less(h.t, elapsed, 30*time.Second, "peer failure must fall back within the bounded read deadline")
	after = h.peerMetrics(readerURL, "after-peer-fallback")
	require.Equal(h.t, before["racer_peer_hits_total"], after["racer_peer_hits_total"])
	require.Equal(h.t, uint64(1), after["racer_origin_fills_total"]-before["racer_origin_fills_total"], "cold reader must acquire the unavailable peer's page from origin")

	gets, _ = fixture.counts()
	require.Equal(h.t, map[string]int{"": 1, "bytes=16777216-": 2, "bytes=33554432-": 1}, gets, "fallback must fetch only the requested page once")
	rules := h.run("docker", "exec", server.name, "iptables", "-L", "FORWARD", "-v", "-n", "-x")
	h.write("peer-outage-iptables.log", rules)

	var rejected uint64

	for _, line := range strings.Split(rules, "\n") {
		if strings.Contains(line, "racer-e2e-peer-outage") {
			var err error

			rejected, err = strconv.ParseUint(strings.Fields(line)[0], 10, 64)
			require.NoError(h.t, err)
		}
	}

	require.Positive(h.t, rejected, "the cold-page read must actually contact the interrupted peer")

	h.run("docker", append([]string{"exec", server.name, "iptables", "-D"}, rule...)...)

	blocked = false
	before = after

	h.readPeerPage(reader, fixture, 2)
	after = h.peerMetrics(readerURL, "after-peer-recovery")
	require.Equal(h.t, uint64(1), after["racer_peer_hits_total"]-before["racer_peer_hits_total"], "healed peer must serve a third page that is still cold on reader")
	require.Equal(h.t, before["racer_origin_fills_total"], after["racer_origin_fills_total"])

	recoveredGets, heads := fixture.counts()
	require.Equal(h.t, gets, recoveredGets, "healed peer read must not fetch origin data")
	h.waitHTTP(serverURL + "/readyz")
	h.waitHTTP(readerURL + "/readyz")
	h.t.Logf("peer reuse and recovery: 2 peer pages, 1 fallback page in %s, %d rejected packets, origin GETs=%v HEADs=%d", elapsed, rejected, recoveredGets, heads)
}

func (h *harness) readPeerPage(node peerNode, fixture *peerOrigin, page int) {
	h.t.Helper()

	first := page * peerPageSize
	last := min(first+peerPageSize, len(fixture.body)) - 1
	metadata := `{"version":1,"registry":"` + registry + `","repository":"fixture/peers","kind":"blob"}`

	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	// kind's node image includes curl. Use the actual client UDS, same UID as
	// Gantry, and explicit pins to distinguish metadata HEADs from data GETs.
	raw := h.command(ctx, "docker", "exec", node.name, "curl", "--include", "--silent", "--show-error", "--fail", "--max-time", "25", "--noproxy", "*", "--unix-socket", "/run/racer/gantry/client/socket", "-H", "Host: racer", "-H", "If-Match: \""+fixture.id+"\"", "-H", "Range: bytes="+strconv.Itoa(first)+"-"+strconv.Itoa(last), "-H", "Racer-Metadata: "+metadata, "http://racer/v1/objects/"+strings.TrimPrefix(fixture.id, "sha256:"))
	response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	require.NoError(h.t, err)

	defer response.Body.Close()

	require.Equal(h.t, http.StatusPartialContent, response.StatusCode)
	require.Equal(h.t, "\""+fixture.id+"\"", response.Header.Get("ETag"))
	require.Equal(h.t, "bytes "+strconv.Itoa(first)+"-"+strconv.Itoa(last)+"/"+strconv.Itoa(len(fixture.body)), response.Header.Get("Content-Range"))
	body, err := io.ReadAll(response.Body)
	require.NoError(h.t, err)
	require.Len(h.t, body, last-first+1)
	require.True(h.t, bytes.Equal(fixture.body[first:last+1], body), "node %s page %d returned incorrect bytes (digest %s)", node.name, page, digest(body))
}

func (h *harness) peerMetrics(endpoint, phase string) map[string]uint64 {
	h.t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(endpoint + "/metrics")
	require.NoError(h.t, err)

	defer response.Body.Close()

	require.Equal(h.t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(h.t, err)
	h.write("peer-metrics-"+phase+".log", string(body))

	metrics := make(map[string]uint64)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.HasPrefix(fields[0], "racer_") {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			require.NoError(h.t, err)

			metrics[fields[0]] = value
		}
	}

	require.NoError(h.t, scanner.Err())

	for _, name := range []string{"racer_peer_hits_total", "racer_origin_fills_total"} {
		require.Contains(h.t, metrics, name, "missing metric must not silently become zero")
	}

	return metrics
}

func TestPeerPlacementFixtureVector(t *testing.T) {
	// placement.rs's fixed samples for cache-a/page 0, equal shares:
	// node-000000=e41095812e885f6f, node-000001=c4251196ce419070.
	var key [32]byte
	for i := range key {
		key[i] = 0x42
	}

	nodes := [2]peerNode{{uid: "node-000000"}, {uid: "node-000001"}}
	require.True(t, peerRanksFirst("cache-a", key, 0, nodes))
	require.False(t, peerRanksFirst("cache-a", key, 0, [2]peerNode{nodes[1], nodes[0]}))
}

func TestPeerPlacementQuantizedCosts(t *testing.T) {
	// Golden costs from topology/placement.rs, plus the integer-log boundaries.
	for _, vector := range []struct{ sample, cost uint64 }{
		{0xe41095812e885f6f, 715971622},
		{0xc4251196ce419070, 1650232626},
		{0x9322018f0806e768, 3431784333},
		{0xb379deaba20d903a, 2200536977},
		{0, 64 << 32},
		{^uint64(0), 1},
		{^uint64(0) - 1, 1},
	} {
		require.Equal(t, vector.cost, peerExponentialCost(vector.sample), "sample %x", vector.sample)
	}
	// Different samples with the same quantized cost must use sorted node IDs,
	// not the greatest raw sample. The old fixture selector got this case wrong.
	samples := [2]uint64{^uint64(0), ^uint64(0) - 1}
	require.False(t, peerScoreFirst(samples, [2]string{"node-b", "node-a"}))
	require.True(t, peerScoreFirst(samples, [2]string{"node-a", "node-b"}))
}

func TestPeerOriginAccounting(t *testing.T) {
	origin := &peerOrigin{body: []byte("exact fixture bytes"), id: digest([]byte("exact fixture bytes")), gets: make(map[string]int)}

	path := "/v2/fixture/peers/blobs/" + origin.id
	for _, test := range []struct {
		method, path, rangeHeader, body string
		status                          int
	}{
		{http.MethodHead, path, "", "", http.StatusOK},
		{http.MethodGet, path, "", string(origin.body), http.StatusOK},
		{http.MethodGet, path, "bytes=6-", "fixture bytes", http.StatusPartialContent},
		{http.MethodGet, path + "-missing", "", "404 page not found\n", http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, path, nil)

		request.URL.Path = test.path
		if test.rangeHeader != "" {
			request.Header.Set("Range", test.rangeHeader)
		}

		response := httptest.NewRecorder()
		origin.ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code)
		require.Equal(t, test.body, response.Body.String())
	}

	gets, heads := origin.counts()
	require.Equal(t, map[string]int{"": 1, "bytes=6-": 1}, gets)
	require.Equal(t, 1, heads, "metadata requests must not count as data fetches")

	gets[""]++
	unchanged, _ := origin.counts()
	require.Equal(t, 1, unchanged[""], "baselines must be detached snapshots")
}
