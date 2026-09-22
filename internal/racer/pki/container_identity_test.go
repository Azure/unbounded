// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import "testing"

func TestContainerIdentityEnrichment(t *testing.T) {
	for _, id := range []Identity{node("node-pod", "boot"), {Kind: ControlPlane, PodUID: "cp-pod", BootID: "boot"}} {
		t.Run(id.Kind, func(t *testing.T) {
			f := newFixture(t)
			issued := f.issue(id, false)

			uri, err := id.URI()
			if err != nil {
				t.Fatal(err)
			}

			enriched := id
			enriched.ContainerID = "containerd://runtime-id"

			wrong := enriched
			if id.Kind == Node {
				wrong.Universe = wrong.Node
			} else {
				wrong.Kind = Node
			}

			if err := f.m.Admit(t.Context(), wrong); err == nil {
				t.Fatal("enrichment changed remaining identity")
			}

			if got := f.state().Members[id.Key().String()].Identity; got != id {
				t.Fatal("failed enrichment changed persisted identity")
			}

			if err := f.m.Admit(t.Context(), enriched); err != nil {
				t.Fatal(err)
			}

			if got, err := enriched.URI(); err != nil || got != uri || enriched.Key() != id.Key() {
				t.Fatal("container metadata changed TLS identity or member key")
			}

			f.issue(enriched, false)

			certs, err := parseCertificates(issued.CertificatePEM)
			if err != nil {
				t.Fatal(err)
			}

			if err := f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err != nil {
				t.Fatal(err)
			}

			restarted, err := New(f.c, "racer", f.m.options)
			if err != nil {
				t.Fatal(err)
			}

			if err := restarted.AcquireLeadership(t.Context(), "next-term"); err != nil {
				t.Fatal(err)
			}

			members, err := restarted.Members(t.Context())
			if err != nil || len(members) != 1 || members[0] != enriched {
				t.Fatalf("container identity did not persist: %v %v", members, err)
			}

			if err := restarted.Admit(t.Context(), enriched); err != nil {
				t.Fatal(err)
			}

			replacement := enriched

			replacement.ContainerID = "containerd://replacement"
			for _, invalid := range []Identity{id, replacement} {
				if err := restarted.Admit(t.Context(), invalid); err == nil {
					t.Fatal("cleared or replaced container identity")
				}
			}

			if got := f.state().Members[id.Key().String()].Identity; got != enriched {
				t.Fatal("rejected change altered durable identity")
			}
		})
	}
}

func TestContainerIdentityAtInitialIssuance(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	id.ContainerID = "containerd://initial"
	f.issue(id, false)

	if got := f.state().Members[id.Key().String()].Identity; got != id {
		t.Fatal("issuance omitted container metadata")
	}

	csr, _ := csrKey(t)

	id.ContainerID = "containerd://replacement"
	if _, err := f.m.Issue(t.Context(), csr, id); err == nil {
		t.Fatal("issuance replaced container metadata")
	}
}
