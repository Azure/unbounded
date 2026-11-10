// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/gantry/progress"
)

const collectionConcurrency = 256

type collectionUpdates struct {
	start func(Snapshot)
	node  func(*Node)
}

type LayerProgress struct {
	Layer
	Available *int64 `json:"available"`
	Committed bool   `json:"committed"`
}

type Node struct {
	Name             string            `json:"name"`
	Phase            string            `json:"phase"`
	Reason           string            `json:"reason"`
	NodeReady        bool              `json:"nodeReady"`
	Pod              string            `json:"pod"`
	PodUID           string            `json:"podUID"`
	Created          *time.Time        `json:"created"`
	Started          *time.Time        `json:"started"`
	ReadyAt          *time.Time        `json:"readyAt"`
	Scheduled        *time.Time        `json:"scheduled"`
	Restarts         int32             `json:"restarts"`
	Layers           []LayerProgress   `json:"layers,omitempty"`
	Available        *int64            `json:"available"`
	TelemetryError   string            `json:"telemetryError"`
	UpdatedAt        *time.Time        `json:"updatedAt"`
	Traffic          *progress.Traffic `json:"traffic"`
	PeerRate         *float64          `json:"peerRate"`
	OriginRate       *float64          `json:"originRate"`
	Storage          *progress.Storage `json:"storage,omitempty"`
	StorageUpdatedAt *time.Time        `json:"storageUpdatedAt"`
	StorageError     string            `json:"storageError,omitempty"`
}

type Snapshot struct {
	Deployment Deployment       `json:"deployment"`
	Nodes      []*Node          `json:"nodes"`
	ObservedAt time.Time        `json:"observedAt"`
	Activity   *ClusterActivity `json:"activity,omitempty"`
}

func (snapshot Snapshot) grid() Snapshot {
	result := snapshot

	result.Nodes = make([]*Node, len(snapshot.Nodes))
	for i, node := range snapshot.Nodes {
		copy := *node
		copy.Layers = nil
		result.Nodes[i] = &copy
	}

	return result
}

func (s *Server) node(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.latestSnapshot(r.PathValue("id"))
	if !ok {
		var err error

		snapshot, err = s.snapshot(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}
	}

	for _, node := range snapshot.Nodes {
		if node.Name == r.PathValue("node") {
			s.write(w, http.StatusOK, node)
			return
		}
	}

	s.fail(w, http.StatusNotFound, errors.New("worker not found in this deployment"))
}

func (s *Server) latestSnapshot(id string) (Snapshot, bool) {
	s.mu.Lock()

	cached := s.snapshots[id]
	if cached != nil {
		cached.usedAt = time.Now()
	}
	s.mu.Unlock()

	if cached == nil {
		return Snapshot{}, false
	}

	cached.mu.RLock()
	defer cached.mu.RUnlock()

	snapshot := cached.snapshot
	snapshot.Nodes = slices.Clone(snapshot.Nodes)

	return snapshot, !cached.at.IsZero()
}

func podNode(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}

	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if required != nil {
			for _, term := range required.NodeSelectorTerms {
				for _, field := range term.MatchFields {
					if field.Key == "metadata.name" && field.Operator == corev1.NodeSelectorOpIn && len(field.Values) == 1 {
						return field.Values[0]
					}
				}
			}
		}
	}

	return ""
}

func ownedPod(pod *corev1.Pod, deployment Deployment) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" && string(owner.UID) == deployment.UID &&
			owner.Name == deployment.Name && owner.Controller != nil && *owner.Controller {
			return true
		}
	}

	return false
}

func (s *Server) snapshot(ctx context.Context, id string) (Snapshot, error) {
	cached, err := s.snapshotEntry(id)
	if err != nil {
		return Snapshot{}, err
	}

	return s.refreshSnapshot(ctx, id, cached, false)
}

func (s *Server) snapshotEntry(id string) (*snapshotCache, error) {
	if !validID.MatchString(id) {
		return nil, errors.New("invalid deployment ID")
	}

	s.mu.Lock()

	cached := s.snapshots[id]
	if cached == nil {
		if len(s.snapshots) >= 64 {
			oldestID := ""

			var oldest time.Time

			for key, entry := range s.snapshots {
				entry.mu.RLock()
				active := entry.subscribers > 0
				entry.mu.RUnlock()

				if active {
					continue
				}

				if oldestID == "" || entry.usedAt.Before(oldest) {
					oldestID, oldest = key, entry.usedAt
				}
			}

			if oldestID == "" {
				s.mu.Unlock()
				return nil, errors.New("too many active image streams")
			}

			delete(s.snapshots, oldestID)
		}

		cached = &snapshotCache{}
		s.snapshots[id] = cached
	}

	cached.usedAt = time.Now()
	s.mu.Unlock()

	return cached, nil
}

func (s *Server) refreshSnapshot(ctx context.Context, id string, cached *snapshotCache, force bool) (Snapshot, error) {
	cached.collectMu.Lock()
	defer cached.collectMu.Unlock()

	cached.mu.RLock()

	if !force && time.Since(cached.at) < s.cfg.Interval {
		snapshot := cached.snapshot
		snapshot.Nodes = slices.Clone(snapshot.Nodes)

		cached.mu.RUnlock()

		return snapshot, nil
	}

	previous := cached.snapshot
	previous.Nodes = slices.Clone(previous.Nodes)

	cached.mu.RUnlock()

	indices := map[string]int{}

	prior := map[string]*Node{}
	for _, node := range previous.Nodes {
		prior[node.Name] = node
	}

	updates := &collectionUpdates{
		start: func(snapshot Snapshot) {
			initial := snapshot

			initial.Nodes = make([]*Node, len(snapshot.Nodes))
			for index, node := range snapshot.Nodes {
				copy := *node

				copy.Layers = slices.Clone(node.Layers)
				if old := prior[node.Name]; old != nil {
					retainByteTelemetry(&copy, old)
					copy.TelemetryError = old.TelemetryError
					copy.Storage, copy.StorageUpdatedAt, copy.StorageError = old.Storage, old.StorageUpdatedAt, old.StorageError
				}

				initial.Nodes[index] = &copy
				indices[node.Name] = index
			}

			cached.mu.Lock()
			cached.snapshot, cached.at, cached.err = initial, time.Now(), nil
			cached.mu.Unlock()
		},
		node: func(node *Node) {
			if ctx.Err() != nil {
				return
			}

			cached.mu.Lock()
			cached.snapshot.Nodes[indices[node.Name]] = node
			cached.snapshot.ObservedAt = time.Now().UTC()
			cached.mu.Unlock()
		},
	}

	snapshot, err := s.collectSnapshot(ctx, id, previous, updates)
	if err != nil {
		return Snapshot{}, err
	}

	cached.mu.Lock()
	snapshot.ObservedAt = time.Now().UTC()
	cached.snapshot, cached.at, cached.err = snapshot, time.Now(), nil
	cached.mu.Unlock()

	return snapshot, nil
}

func (s *Server) collectSnapshot(ctx context.Context, id string, previous Snapshot, updates *collectionUpdates) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	deployment, err := s.deployment(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}

	nodes, err := s.cfg.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.azure.com/agentpool=" + s.cfg.NodePool +
			",kubernetes.azure.com/nodepool-type=FlexNodes,kubernetes.io/os=linux,kubernetes.io/arch=" + s.cfg.Architecture,
	})
	if err != nil {
		return Snapshot{}, err
	}

	pods, err := s.cfg.Client.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: deploymentLabel + "=" + id,
	})
	if err != nil {
		return Snapshot{}, err
	}

	agents, err := s.cfg.Client.CoreV1().Pods(s.cfg.GantryNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=gantry",
	})
	if err != nil {
		return Snapshot{}, err
	}

	podByNode := map[string]*corev1.Pod{}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedPod(pod, deployment) {
			continue
		}

		name := podNode(pod)

		current := podByNode[name]
		if current == nil || pod.CreationTimestamp.After(current.CreationTimestamp.Time) {
			podByNode[name] = pod
		}
	}

	agentByNode := map[string]*corev1.Pod{}

	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.DeletionTimestamp != nil || agent.Spec.NodeName == "" {
			continue
		}

		for _, container := range agent.Spec.Containers {
			if container.Name == "gantry" {
				agentByNode[agent.Spec.NodeName] = agent
			}
		}
	}

	s.setAgentRoster(agentByNode)

	old := map[string]*Node{}
	for _, node := range previous.Nodes {
		old[node.Name] = node
	}

	snapshot := Snapshot{Deployment: deployment, Nodes: []*Node{}, ObservedAt: time.Now().UTC()}
	gridNodes := make(map[string]bool, len(nodes.Items))

	for _, n := range nodes.Items {
		gridNodes[n.Name] = true
		pod := podByNode[n.Name]
		phase, reason := podLifecycle(pod)

		node := &Node{Name: n.Name, Phase: phase, Reason: reason, Layers: []LayerProgress{}}
		for _, condition := range n.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				node.NodeReady = condition.Status == corev1.ConditionTrue
			}
		}

		if pod != nil {
			node.Pod, node.PodUID, node.Created = pod.Name, string(pod.UID), &pod.CreationTimestamp.Time
			for _, status := range pod.Status.ContainerStatuses {
				node.Restarts += status.RestartCount
				if status.State.Running != nil {
					node.Started = &status.State.Running.StartedAt.Time
				}
			}

			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
					node.Scheduled = &condition.LastTransitionTime.Time
				}

				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					node.ReadyAt = &condition.LastTransitionTime.Time
				}
			}
		}

		for _, layer := range deployment.Image.Layers {
			node.Layers = append(node.Layers, LayerProgress{Layer: layer})
		}

		snapshot.Nodes = append(snapshot.Nodes, node)
	}

	sort.Slice(snapshot.Nodes, func(i, j int) bool { return snapshot.Nodes[i].Name < snapshot.Nodes[j].Name })

	if updates != nil {
		updates.start(snapshot)
	}

	group := new(errgroup.Group)
	group.SetLimit(collectionConcurrency)

	for _, node := range snapshot.Nodes {
		group.Go(func() error {
			defer func() {
				prior := old[node.Name]
				if node.TelemetryError != "" {
					if prior == nil || prior.TelemetryError != node.TelemetryError {
						s.cfg.Logger.Warn("Gantry agent progress unavailable",
							slog.String("node", node.Name), slog.String("error", node.TelemetryError))
					}

					retainByteTelemetry(node, prior)

					if node.Storage == nil && node.StorageError == "" {
						node.StorageError = node.TelemetryError
					}
				}

				if node.Storage == nil && prior != nil {
					node.Storage, node.StorageUpdatedAt = prior.Storage, prior.StorageUpdatedAt
				}

				if node.StorageError != "" && node.TelemetryError == "" && (prior == nil || prior.StorageError != node.StorageError) {
					s.cfg.Logger.Warn("Gantry filesystem usage unavailable",
						slog.String("node", node.Name), slog.String("error", node.StorageError))
				}

				if updates != nil {
					updates.node(node)
				}
			}()

			select {
			case s.collect <- struct{}{}:
				defer func() { <-s.collect }()
			case <-ctx.Done():
				node.TelemetryError = "Collection deadline reached; no current byte observation"
				return nil
			}

			agent := agentByNode[node.Name]
			if agent == nil {
				node.TelemetryError = "No Gantry agent found on this node"
				return nil
			}

			response, err := s.agentProgress(ctx, agent, deployment.Image.Layers)
			s.recordActivity(agent, response, time.Now().UTC(), err != nil)

			if err != nil {
				node.TelemetryError = err.Error()
				return nil
			}

			now := time.Now().UTC()
			applyStorage(node, response, now)

			if err := applyProgress(node, response); err != nil {
				node.TelemetryError = err.Error()
				return nil
			}

			node.UpdatedAt, node.Traffic = &now, &response.Traffic
			if prior := old[node.Name]; prior != nil && prior.Traffic != nil && prior.UpdatedAt != nil {
				seconds := now.Sub(*prior.UpdatedAt).Seconds()
				if seconds > 0 && response.Traffic.PeerBytes >= prior.Traffic.PeerBytes &&
					response.Traffic.OriginBytes >= prior.Traffic.OriginBytes {
					peer := (response.Traffic.PeerBytes - prior.Traffic.PeerBytes) / seconds
					origin := (response.Traffic.OriginBytes - prior.Traffic.OriginBytes) / seconds
					node.PeerRate, node.OriginRate = &peer, &origin
				}
			}

			return nil
		})
	}

	for name, agent := range agentByNode {
		if gridNodes[name] {
			continue
		}

		group.Go(func() error {
			select {
			case s.collect <- struct{}{}:
				defer func() { <-s.collect }()
			case <-ctx.Done():
				return nil
			}

			response, err := s.agentProgress(ctx, agent, nil)
			s.recordActivity(agent, response, time.Now().UTC(), err != nil)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return Snapshot{}, err
	}

	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	return snapshot, nil
}

func retainByteTelemetry(node, prior *Node) {
	if prior == nil || prior.Available == nil {
		return
	}

	node.Available, node.UpdatedAt, node.Traffic = prior.Available, prior.UpdatedAt, prior.Traffic
	node.PeerRate, node.OriginRate = prior.PeerRate, prior.OriginRate
	node.Layers = slices.Clone(prior.Layers)
}

func applyStorage(node *Node, response progress.Response, at time.Time) {
	if response.StorageError != "" {
		node.StorageError = response.StorageError
		return
	}

	storage := response.Storage
	if storage == nil {
		node.StorageError = "agent upgrade required: no filesystem telemetry"
		return
	}

	if storage.Total == 0 || storage.Used > storage.Total || storage.Available > storage.Total-storage.Used {
		node.StorageError = "agent returned inconsistent filesystem usage"
		return
	}

	node.Storage, node.StorageUpdatedAt = storage, &at
	node.StorageError = ""
}

func applyProgress(node *Node, response progress.Response) error {
	byDigest := map[string]int{}
	for i, blob := range response.Blobs {
		if _, exists := byDigest[blob.Digest]; exists {
			return errors.New("agent returned duplicate layer progress")
		}

		byDigest[blob.Digest] = i
	}

	var available int64

	layers := slices.Clone(node.Layers)

	for i, layer := range layers {
		index, ok := byDigest[layer.Digest]
		if !ok {
			return errors.New("agent did not report every requested layer")
		}

		blob := response.Blobs[index]
		if blob.Available < 0 || blob.Available > layer.Size || (blob.Complete && blob.Available != layer.Size) {
			return errors.New("agent returned inconsistent layer availability")
		}

		value := blob.Available
		layers[i].Available, layers[i].Committed = &value, blob.Complete
		available += value
	}

	node.Layers, node.Available = layers, &available

	return nil
}

func (s *Server) agentProgress(ctx context.Context, agent *corev1.Pod, layers []Layer) (progress.Response, error) {
	digests := make([]string, len(layers))
	for i, layer := range layers {
		digests[i] = layer.Digest
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var body []byte

	if !s.cfg.DirectAgents {
		client := s.cfg.Client.CoreV1().RESTClient()
		if client == nil {
			return progress.Response{}, errors.New("agent proxy is unavailable")
		}

		request := client.Get().Namespace(s.cfg.GantryNamespace).Resource("pods").
			Name(agent.Name + ":9095").SubResource("proxy").Suffix("v1/progress")
		if len(digests) > 0 {
			request.Param("digest", strings.Join(digests, ","))
		}

		result := request.Do(ctx)

		var status int
		result.StatusCode(&status)

		if status == http.StatusNotFound {
			return progress.Response{}, errors.New("agent upgrade required: no image-progress endpoint (Racer is not supported)")
		}

		var err error

		body, err = result.Raw()
		if err != nil {
			return progress.Response{}, fmt.Errorf("agent progress unavailable (proxy HTTP %d)", status)
		}
	} else {
		if net.ParseIP(agent.Status.PodIP) == nil {
			return progress.Response{}, errors.New("agent has no routable Pod IP")
		}

		u := url.URL{Scheme: "http", Host: net.JoinHostPort(agent.Status.PodIP, "9095"), Path: "/v1/progress"}
		if len(digests) > 0 {
			u.RawQuery = url.Values{"digest": {strings.Join(digests, ",")}}.Encode()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return progress.Response{}, err
		}

		resp, err := s.http.Do(req)
		if err != nil {
			return progress.Response{}, errors.New("cannot reach Gantry agent progress endpoint")
		}

		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response cleanup

		if resp.StatusCode == http.StatusNotFound {
			return progress.Response{}, errors.New("agent upgrade required: no image-progress endpoint (Racer is not supported)")
		}

		if resp.StatusCode != http.StatusOK {
			return progress.Response{}, fmt.Errorf("agent progress unavailable (HTTP %d)", resp.StatusCode)
		}

		body, err = io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		if err != nil {
			return progress.Response{}, err
		}
	}

	var response progress.Response
	if err := json.Unmarshal(body, &response); err != nil {
		return progress.Response{}, errors.New("invalid agent progress response")
	}

	if err := validateAgentActivity(response); err != nil {
		return progress.Response{}, err
	}

	return response, nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	deployment, err := s.deployment(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}

	pods, err := s.cfg.Client.CoreV1().Pods(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: deploymentLabel + "=" + deployment.ID,
	})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	var pod *corev1.Pod

	for i := range pods.Items {
		candidate := &pods.Items[i]
		if ownedPod(candidate, deployment) && podNode(candidate) == r.URL.Query().Get("node") &&
			(pod == nil || candidate.CreationTimestamp.After(pod.CreationTimestamp.Time)) {
			pod = candidate
		}
	}

	if pod == nil {
		s.write(w, http.StatusOK, []any{})
		return
	}

	events, err := s.cfg.Client.CoreV1().Events(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{
		FieldSelector: "involvedObject.uid=" + string(pod.UID),
	})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	type event struct {
		Reason  string    `json:"reason"`
		Message string    `json:"message"`
		Type    string    `json:"type"`
		At      time.Time `json:"at"`
		Count   int32     `json:"count"`
		FirstAt time.Time `json:"firstAt"`
		PodUID  string    `json:"podUID"`
	}

	result := []event{}

	for _, item := range events.Items {
		if item.InvolvedObject.UID != pod.UID {
			continue
		}

		at := item.LastTimestamp.Time
		if at.IsZero() {
			at = item.EventTime.Time
		}

		if at.IsZero() {
			at = item.CreationTimestamp.Time
		}

		count := max(int32(1), item.Count)
		if item.Series != nil {
			count = max(count, item.Series.Count)
			if item.Series.LastObservedTime.After(at) {
				at = item.Series.LastObservedTime.Time
			}
		}

		first := item.FirstTimestamp.Time
		if first.IsZero() {
			first = item.EventTime.Time
		}

		if first.IsZero() {
			first = item.CreationTimestamp.Time
		}

		result = append(result, event{Reason: item.Reason, Message: item.Message, Type: item.Type, At: at, Count: count, FirstAt: first, PodUID: string(pod.UID)})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	s.write(w, http.StatusOK, result)
}
