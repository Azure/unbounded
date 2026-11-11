// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type nodeUpdates struct {
	Nodes      []*Node          `json:"nodes"`
	ObservedAt time.Time        `json:"observedAt"`
	Activity   *ClusterActivity `json:"activity,omitempty"`
	History    *RunHistory      `json:"history,omitempty"`
}

func (s *Server) streamUpdates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	cached, err := s.snapshotEntry(id)
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}

	cached.mu.Lock()

	cached.subscribers++
	if cached.ctx == nil || cached.ctx.Err() != nil {
		ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))

		cached.ctx, cached.cancel = ctx, cancel
		go s.runCollection(ctx, id, cached)
	}

	cached.mu.Unlock()
	defer func() {
		cached.mu.Lock()

		cached.subscribers--
		cached.mu.Unlock()
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	previous := map[string]*Node{}

	var (
		uid, lastError   string
		activityRevision uint64
	)

	initialized := false
	historyPoints := 0

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := writeStreamEvent(w, "ping", struct{}{}); err != nil {
				return
			}
		case <-ticker.C:
			cached.mu.RLock()
			snapshot, failure := cached.snapshot, cached.err
			snapshot.Nodes = slices.Clone(snapshot.Nodes)

			cached.mu.RUnlock()

			if failure != nil {
				if failure.Error() != lastError {
					lastError = failure.Error()
					if err := writeStreamEvent(w, "status-error", map[string]string{"error": lastError}); err != nil {
						return
					}
				}

				continue
			}

			if snapshot.Deployment.ID == "" {
				continue
			}

			snapshot.Activity = s.clusterActivity(time.Now())
			snapshot.History = s.recordRunHistory(cached, time.Now().UTC())

			lastError = ""
			full := !initialized || uid != snapshot.Deployment.UID || len(previous) != len(snapshot.Nodes)
			changed := make([]*Node, 0)

			current := make(map[string]*Node, len(snapshot.Nodes))
			for _, node := range snapshot.Nodes {
				current[node.Name] = node
				if _, exists := previous[node.Name]; !exists {
					full = true
				}

				if previous[node.Name] != node {
					copy := *node
					copy.Layers = nil
					changed = append(changed, &copy)
				}
			}

			if full {
				snapshot.History = cloneRunHistory(snapshot.History, 0)
				if err := writeStreamEvent(w, "snapshot", snapshot.grid()); err != nil {
					return
				}

				initialized, uid = true, snapshot.Deployment.UID
			} else if len(changed) > 0 || activityRevision != snapshot.Activity.Revision || snapshot.History != nil && len(snapshot.History.Points) > historyPoints {
				var history *RunHistory
				if snapshot.History != nil && len(snapshot.History.Points) > historyPoints {
					history = cloneRunHistory(snapshot.History, historyPoints)
				}

				if err := writeStreamEvent(w, "node-updates", nodeUpdates{Nodes: changed, ObservedAt: snapshot.ObservedAt, Activity: snapshot.Activity, History: history}); err != nil {
					return
				}
			}

			previous = current

			activityRevision = snapshot.Activity.Revision
			if snapshot.History != nil {
				historyPoints = len(snapshot.History.Points)
			}
		}
	}
}

func writeStreamEvent(w http.ResponseWriter, event string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}

	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}

	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return err
	}

	if err := controller.Flush(); err != nil {
		return err
	}

	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}

	return nil
}

func (s *Server) runCollection(ctx context.Context, id string, cached *snapshotCache) {
	for ctx.Err() == nil {
		started := time.Now()

		_, err := s.refreshSnapshot(ctx, id, cached, true)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			s.cfg.Logger.Warn("UI collection failed", slog.Any("error", err))
			cached.mu.Lock()
			cached.err = err
			cached.mu.Unlock()

			if apierrors.IsNotFound(err) {
				cached.cancel()
				return
			}
		} else {
			s.recordRunHistory(cached, time.Now().UTC())
		}

		timer := time.NewTimer(max(10*time.Millisecond, s.cfg.Interval-time.Since(started)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
