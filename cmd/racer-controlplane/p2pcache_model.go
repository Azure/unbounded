// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

// All caches in a Site share the same managed process inventory. Peer ports
// remain allocated after withdrawal so a returning cache cannot collide with a
// draining listener. Local sockets are derived separately from the cache name.
func buildCacheGeneration(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod, caches []racerapi.P2PCache, reserved reservedPorts, root string) (*generation, error) {
	if name == "" {
		return nil, fmt.Errorf("universe must not be empty")
	}

	ports := map[string]int32{}
	used := map[int32]bool{}

	if previous != nil {
		for id, port := range previous.Ports {
			ports[id], used[port] = port, true
		}
	}

	caches = append([]racerapi.P2PCache(nil), caches...)
	sort.Slice(caches, func(i, j int) bool { return caches[i].Name < caches[j].Name })

	g, err := buildInventory(name, previous, nodes, pods, ports)
	if err != nil {
		return nil, err
	}

	g.Withdrawn = map[string]bool{}
	if previous != nil {
		for id, withdrawn := range previous.Withdrawn {
			g.Withdrawn[id] = withdrawn
		}

		for _, old := range previous.volumes() {
			g.Withdrawn[old.Volume.ID] = true
		}
	}

	var active []string

	for node, member := range g.Nodes {
		if member.IP != "" {
			active = append(active, node)
		}
	}

	nextPort := int32(10000)

	for _, cache := range caches {
		if cache.UID == "" || cache.Spec.CacheGeneration < 0 || cache.Spec.MaxCandidateAttempts < 1 || cache.Spec.MaxCandidateAttempts > 8 {
			return nil, fmt.Errorf("P2PCache %s has invalid identity or configuration", cache.Name)
		}

		local, origin, err := racer.CacheSockets(root, cache.Name)
		if err != nil {
			return nil, fmt.Errorf("P2PCache %s: %w", cache.Name, err)
		}

		id := string(cache.UID)
		delete(g.Withdrawn, id)
		// The socket and peer-port lifetime follows the name. Cache identity follows
		// the UID, ensuring recreation cannot reuse cached bytes from an old object.
		port := ports[cache.Name]
		if port == 0 {
			for nextPort <= 29999 && (used[nextPort] || reserved.contains(nextPort)) {
				nextPort++
			}

			if nextPort > 29999 {
				return nil, fmt.Errorf("peer listener port range exhausted")
			}

			port = nextPort
			ports[cache.Name], used[port] = port, true
		}

		if reserved.contains(port) {
			return nil, fmt.Errorf("historical peer listener port %d for %s is reserved for management", port, cache.Name)
		}

		g.Ports[cache.Name] = port
		v := &volumeSpec{ID: id, Name: cache.Name, ResourceGeneration: cache.Generation, CacheSocket: local, OriginSocket: origin, Port: port, Slots: racer.SlotCount, Cache: uint64(cache.Spec.CacheGeneration), Algorithm: 2, Attempts: uint32(cache.Spec.MaxCandidateAttempts)}

		var priorOwners []string

		if previous != nil {
			for _, old := range previous.volumes() {
				if old.Volume.ID == id {
					priorOwners = old.Owners
				}
			}
		}

		var owners []string
		if len(active) != 0 {
			owners, err = place(v.Slots, active, priorOwners)
			if err != nil {
				return nil, err
			}
		}

		g.SlotHistory[id] = v.Slots
		if g.Volume == nil {
			g.Volume, g.Owners = v, owners
		} else {
			g.Additional = append(g.Additional, volumeState{v, owners})
		}
	}

	if len(g.Withdrawn) == 0 {
		g.Withdrawn = nil
	}

	return g, nil
}
