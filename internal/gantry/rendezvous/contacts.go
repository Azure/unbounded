// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"encoding/json"
	"fmt"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func (m *Manager) contactsFromLease(lease *coordinationv1.Lease) ([]Contact, error) {
	freshness := m.leaseFreshness(lease)
	if freshness == "expired" || holder(lease) == "" {
		return nil, nil
	}

	holderID, err := peer.Decode(holder(lease))
	if err != nil {
		return nil, fmt.Errorf("invalid holder peer ID: %w", err)
	}

	contacts := make([]Contact, 0, m.contactsPerSlot)

	rawPrimary := lease.Annotations[AnnotationP2PAddrs]
	if len(rawPrimary) > m.maxBundleSize {
		return nil, fmt.Errorf("primary address bundle exceeds %d bytes", m.maxBundleSize)
	}

	primary, err := parseAddrList(rawPrimary, m.contactsPerSlot)
	if err != nil {
		return nil, fmt.Errorf("primary addresses: %w", err)
	}

	if len(primary) > 0 {
		for _, info := range primary {
			if info.ID != holderID {
				return nil, fmt.Errorf("holder %s does not match address peer %s", holderID, info.ID)
			}
		}

		contacts = append(contacts, Contact{
			Info:      mergeAddrInfos(primary),
			Slot:      lease.Name,
			Freshness: freshness,
		})
	}

	rawSample := lease.Annotations[AnnotationBootstrapSample]
	if rawSample == "" || len(contacts) >= m.contactsPerSlot {
		return contacts, nil
	}

	if len(rawSample) > m.maxBundleSize {
		return nil, fmt.Errorf("bootstrap sample exceeds %d bytes", m.maxBundleSize)
	}

	var bundle contactBundle
	if err := json.Unmarshal([]byte(rawSample), &bundle); err != nil {
		return nil, fmt.Errorf("bootstrap sample JSON: %w", err)
	}

	if bundle.Version != contactBundleVersion {
		return nil, fmt.Errorf("unsupported bootstrap sample version %d", bundle.Version)
	}

	for _, raw := range bundle.Peers {
		if len(contacts) >= m.contactsPerSlot {
			break
		}

		info, err := peer.AddrInfoFromString(raw)
		if err != nil {
			return nil, fmt.Errorf("sample address: %w", err)
		}

		contacts = append(contacts, Contact{
			Info:      *info,
			Slot:      lease.Name,
			Freshness: freshness,
			Sampled:   true,
		})
	}

	return contacts, nil
}

type contactBundle struct {
	Version int      `json:"version"`
	Peers   []string `json:"peers"`
}

func holder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}

	return *lease.Spec.HolderIdentity
}

func parseAddrList(raw string, limit int) ([]peer.AddrInfo, error) {
	parts := strings.Split(raw, ",")

	result := make([]peer.AddrInfo, 0, min(len(parts), limit))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		info, err := peer.AddrInfoFromString(part)
		if err != nil {
			return nil, err
		}

		result = append(result, *info)
		if len(result) == limit {
			break
		}
	}

	return result, nil
}

func mergeAddrInfos(infos []peer.AddrInfo) peer.AddrInfo {
	result := peer.AddrInfo{ID: infos[0].ID}
	for _, info := range infos {
		if info.ID == result.ID {
			result.Addrs = appendUniqueAddrs(result.Addrs, info.Addrs...)
		}
	}

	return result
}

func appendUniqueAddrs(existing []multiaddr.Multiaddr, addrs ...multiaddr.Multiaddr) []multiaddr.Multiaddr {
	seen := make(map[string]struct{}, len(existing)+len(addrs))
	for _, addr := range existing {
		seen[addr.String()] = struct{}{}
	}

	for _, addr := range addrs {
		if _, ok := seen[addr.String()]; ok {
			continue
		}

		seen[addr.String()] = struct{}{}
		existing = append(existing, addr)
	}

	return existing
}
