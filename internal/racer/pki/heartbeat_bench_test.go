// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"testing"
	"time"
)

// Use real issued leaves and a warm immutable shard cache. Setup is in memory;
// the timed paths are the production verification and observation methods.
func BenchmarkMemberHeartbeat(b *testing.B) {
	for _, count := range []int{1, 1500} {
		b.Run(fmt.Sprintf("members=%d", count), func(b *testing.B) {
			now := time.Now().UTC().Truncate(time.Second)
			options := (Options{Now: func() time.Time { return now }}).defaults()

			ca, err := makeCA(now, options)
			if err != nil {
				b.Fatal(err)
			}

			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				b.Fatal(err)
			}

			s := &state{Version: 2, Fence: "benchmark", FenceAt: now, Generation: 1, Active: ca.Digest, Phase: "stable", Authorities: []authority{ca}, Shards: map[string]shardReference{}}
			m := &Manager{options: options, fence: s.Fence, leaderContext: b.Context(), shardCache: map[string]cachedShard{}}
			ids := make([]MemberKey, count)

			leaves := make([][]byte, count)
			for i := range count {
				id := node(fmt.Sprintf("pod-%08d", i), "boot")

				pem, expiry, err := signLeaf(ca, &x509.CertificateRequest{PublicKey: key.Public()}, id, "racer", now, options)
				if err != nil {
					b.Fatal(err)
				}

				certs, err := parseCertificates(pem)
				if err != nil {
					b.Fatal(err)
				}

				ids[i], leaves[i] = id.Key(), certs[0].Raw
				bucket := participantBucket(id.Key().String())
				name := "racer-pki-benchmark-" + bucket
				ref := shardReference{Name: name}

				cached := m.shardCache[bucket]
				if cached.shard.Members == nil {
					cached = cachedShard{ref: ref, shard: participantShard{Members: map[string]*member{}, Retired: map[string]bool{}}}
				}

				cached.shard.Members[id.Key().String()] = &member{Identity: id, Leaves: map[string]leafRecord{digest(leaves[i]): {Root: ca.Digest, Expiry: expiry}}}
				m.shardCache[bucket] = cached
				s.Shards[bucket] = ref
				s.Authorities[0].LastIssuedExpiry = expiry
			}

			m.localState.Store(s)
			ack := Acknowledgment{Generation: s.Generation, Digest: s.bundle().Digest()}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				index := i % count
				if err := m.VerifyMember(b.Context(), ids[index], leaves[index]); err != nil {
					b.Fatal(err)
				}

				if err := m.ObserveHeartbeat(b.Context(), ids[index], ack); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
