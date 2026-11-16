// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package progress exposes bounded, read-only local image availability.
package progress

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/v4/disk"

	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

const MaxLayers = 512

type Traffic struct {
	OriginBytes float64 `json:"originBytes"`
	PeerBytes   float64 `json:"peerBytes"`
}

type Response struct {
	Blobs        []containerdstore.BlobProgress `json:"blobs"`
	Traffic      Traffic                        `json:"traffic"`
	Activity     *Activity                      `json:"activity,omitempty"`
	Storage      *Storage                       `json:"storage,omitempty"`
	StorageError string                         `json:"storageError,omitempty"`
}

type Storage struct {
	Path      string `json:"path"`
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

type Activity struct {
	OriginActive     int64   `json:"originActive"`
	PeerActive       int64   `json:"peerActive"`
	OriginFailures   float64 `json:"originFailures"`
	PeerBusy         float64 `json:"peerBusy"`
	PeerDialFailures float64 `json:"peerDialFailures"`
}

func NewHandler(store *containerdstore.Store, ranges *rangecache.Cache, gatherer prometheus.Gatherer, storagePath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		var values []string

		if query, present := r.URL.Query()["digest"]; present {
			if len(query) != 1 || query[0] == "" {
				http.Error(w, "invalid layer digest", http.StatusBadRequest)
				return
			}

			values = strings.Split(query[0], ",")
		}

		if len(values) > MaxLayers {
			http.Error(w, "too many layer digests", http.StatusBadRequest)
			return
		}

		digests := make([]digest.Digest, 0, len(values))
		seen := map[string]bool{}

		for _, value := range values {
			d, err := digest.Parse(value)
			if err != nil {
				http.Error(w, "invalid layer digest", http.StatusBadRequest)
				return
			}

			if !seen[value] {
				digests = append(digests, d)
				seen[value] = true
			}
		}

		blobs := []containerdstore.BlobProgress{}

		if len(digests) > 0 {
			var err error

			blobs, err = store.Progress(r.Context(), digests)
			if err != nil {
				http.Error(w, "containerd progress unavailable", http.StatusServiceUnavailable)
				return
			}
		}

		for i := range blobs {
			if ranges != nil && !blobs[i].Complete {
				available, total := ranges.Coverage(digests[i], blobs[i].Available)
				blobs[i].Available = available
				blobs[i].Total = max(blobs[i].Total, total)
			}
		}

		families, err := gatherer.Gather()
		if err != nil {
			http.Error(w, "transfer counters unavailable", http.StatusServiceUnavailable)
			return
		}

		response := Response{Blobs: blobs}

		if storagePath != "" {
			usage, err := disk.UsageWithContext(r.Context(), storagePath)
			if err != nil {
				response.StorageError = "filesystem usage unavailable: " + err.Error()
			} else {
				response.Storage = &Storage{
					Path: storagePath, Total: usage.Total, Used: usage.Used, Available: usage.Free,
				}
			}
		}

		var (
			activity Activity
			live     Traffic
		)

		seenBytes, seenActive := map[string]bool{}, map[string]bool{}

		for _, family := range families {
			for _, metric := range family.Metric {
				kind, source, outcome := "", "", ""

				for _, label := range metric.Label {
					switch label.GetName() {
					case "kind":
						kind = label.GetValue()
					case "source":
						source = label.GetValue()
					case "outcome":
						outcome = label.GetValue()
					}
				}

				value := metric.GetCounter().GetValue()

				switch {
				case family.GetName() == "gantry_transfer_bytes_total" && kind == "layer" && (source == "origin" || source == "peer"):
					seenBytes[source] = true
					if source == "origin" {
						live.OriginBytes += value
					} else {
						live.PeerBytes += value
					}
				case family.GetName() == "gantry_active_transfers" && kind == "layer" && (source == "origin" || source == "peer"):
					active := metric.GetGauge().GetValue()
					if active < 0 || math.IsNaN(active) || math.IsInf(active, 0) || active != math.Trunc(active) || active >= float64(math.MaxInt64) {
						http.Error(w, "invalid transfer activity", http.StatusServiceUnavailable)
						return
					}

					seenActive[source] = true
					if source == "origin" {
						activity.OriginActive += int64(active)
					} else {
						activity.PeerActive += int64(active)
					}
				case family.GetName() == "p2p_origin_failure_total":
					activity.OriginFailures += value
				case family.GetName() == "gantry_streaming_requests_total" && source == "origin" && outcome == "error":
					activity.OriginFailures += value
				case family.GetName() == "p2p_peer_fetch_total" && outcome == "busy":
					activity.PeerBusy += value
				case family.GetName() == "p2p_peer_dial_failure_total":
					activity.PeerDialFailures += value
				case family.GetName() == "gantry_origin_bytes_total" && kind == "layer",
					family.GetName() == "gantry_streaming_bytes_total" && source == "origin":
					response.Traffic.OriginBytes += value
				case family.GetName() == "gantry_peer_fetch_bytes_total" && kind == "layer",
					family.GetName() == "gantry_streaming_bytes_total" && source == "peer":
					response.Traffic.PeerBytes += value
				}
			}

			if seenBytes["origin"] && seenBytes["peer"] && seenActive["origin"] && seenActive["peer"] {
				response.Traffic = live
				response.Activity = &activity
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		if err := json.NewEncoder(w).Encode(response); err != nil {
			return // The client has disconnected; headers are already committed.
		}
	})
}
