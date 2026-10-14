// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMediaSiteLookups(t *testing.T) {
	for _, providerSite := range []string{"site-a", ""} {
		for _, ownerSite := range []string{"site-a", "site-b", ""} {
			t.Run(providerSite+"/"+ownerSite, func(t *testing.T) {
				p, m, op := mediaFixture(t, "amd64")
				p.Site = providerSite
				m.Labels["unbounded-cloud.io/site"] = ownerSite
				require.NoError(t, p.Client.Update(t.Context(), m))

				b := op.Status.Targets[0].BootMedia
				b.Site = ownerSite

				require.NoError(t, p.Client.Status().Update(t.Context(), op))

				matches := providerSite == ownerSite
				ip := b.Netboot.DHCPLeases[0].IPv4

				o, err := p.callbackOwner(t.Context(), b.ID)
				if matches {
					require.NoError(t, err)
					require.Equal(t, op.UID, o.op.UID)
				} else {
					require.Error(t, err)
					require.Nil(t, o)
					_, err = p.Prepare(t.Context(), m, op, b.ID)
					require.ErrorContains(t, err, "no active media owner")
					_, err = p.OpenISO(t.Context(), "ub-"+b.ID+".iso")
					require.ErrorContains(t, err, "no active media owner")
				}

				o, claimed, err := p.artifactOwner(t.Context(), ip)
				require.Equal(t, matches, claimed)

				if matches {
					require.NoError(t, err)
					require.Equal(t, op.UID, o.op.UID)
				} else {
					require.Nil(t, o)
				}

				wantCode := http.StatusNotFound
				if matches {
					wantCode = http.StatusOK
				}

				require.Equal(t, wantCode, mediaRequest(p, b.ID, http.MethodPost, "complete", ip).Code)
				require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
				b = op.Status.Targets[0].BootMedia
				require.Equal(t, matches, b.ImageWritten)

				b.ImageWritten = true
				now := metav1.Now()
				b.RebootRequestedAt = &now

				require.NoError(t, p.Client.Status().Update(t.Context(), op))
				o, claimed, err = p.firstBootOwner(t.Context(), ip)
				require.NoError(t, err)
				require.Equal(t, matches, claimed)

				if matches {
					require.Equal(t, op.UID, o.op.UID)
				} else {
					require.Nil(t, o)
				}
			})
		}
	}
}

func TestMediaSiteIgnoresForeignDuplicate(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	foreign := op.Status.Targets[0].DeepCopy()
	foreign.MachineRef = "foreign-host"
	foreign.BootMedia.Site = "site-b"
	op.Status.Targets = append(op.Status.Targets, *foreign)
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	id := op.Status.Targets[0].BootMedia.ID
	ip := op.Status.Targets[0].BootMedia.Netboot.DHCPLeases[0].IPv4

	o, err := p.callbackOwner(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 0, o.index)
	o, claimed, err := p.artifactOwner(t.Context(), ip)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, 0, o.index)

	now := metav1.Now()

	for i := range op.Status.Targets {
		op.Status.Targets[i].BootMedia.ImageWritten = true
		op.Status.Targets[i].BootMedia.RebootRequestedAt = &now
	}

	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	o, claimed, err = p.firstBootOwner(t.Context(), ip)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, 0, o.index)
}

func TestMediaSiteNonMediaSuccessor(t *testing.T) {
	for _, providerSite := range []string{"site-a", ""} {
		for _, localSuccessor := range []bool{false, true} {
			name := providerSite + "/foreign-only"
			if localSuccessor {
				name = providerSite + "/local-and-foreign"
			}

			t.Run(name, func(t *testing.T) {
				p, m, old := mediaFixture(t, "amd64")
				p.Site = providerSite
				old.Status.Phase = v1alpha3.OperationPhaseComplete
				b := old.Status.Targets[0].BootMedia
				b.Site = providerSite
				b.Cleaned, b.Released, b.ImageWritten = true, true, true

				require.NoError(t, p.Client.Status().Update(t.Context(), old))

				ip := b.Netboot.DHCPLeases[0].IPv4
				m.Labels["unbounded-cloud.io/site"] = providerSite
				m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolHTTP
				foreign := m.DeepCopy()
				foreign.Name, foreign.UID, foreign.ResourceVersion = "foreign-host", "foreign-uid", ""
				foreign.Labels["unbounded-cloud.io/site"] = "site-b"
				require.NoError(t, p.Client.Create(t.Context(), foreign))

				if !localSuccessor {
					m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.99"
				}

				require.NoError(t, p.Client.Update(t.Context(), m))

				for _, machine := range []*v1alpha3.Machine{m, foreign} {
					next := &v1alpha3.MachineOperation{
						ObjectMeta: metav1.ObjectMeta{Name: machine.Name + "-next"},
						Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: machine.Name},
					}
					require.NoError(t, p.Client.Create(t.Context(), next))
				}

				o, claimed, err := p.artifactOwner(t.Context(), ip)
				require.Nil(t, o)
				require.Equal(t, !localSuccessor, claimed)

				if localSuccessor {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
