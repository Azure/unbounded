// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

type tftpReaderFrom struct {
	readerFrom
	ip string
}

func (r *tftpReaderFrom) RemoteAddr() net.UDPAddr {
	return net.UDPAddr{IP: net.ParseIP(r.ip), Port: 4321}
}

func (*tftpReaderFrom) SetSize(int64) {}

func TestTFTPMediaLeaseAuthorization(t *testing.T) {
	for _, scenario := range []string{
		"active media", "released history", "cleanup pending", "successor",
		"unclaimed IP", "no media claim", "unrelated PXE operation", "media client unavailable",
	} {
		t.Run(scenario, func(t *testing.T) {
			p, m, old := mediaFixture(t, "amd64")
			m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
			media := old.Status.Targets[0].BootMedia
			media.Phase = "Installing"
			objects := []client.Object{m, old}
			wantAllowed := false

			switch scenario {
			case "released history", "cleanup pending", "successor":
				old.Status.Phase = v1alpha3.OperationPhaseComplete

				media.Cleaned, media.Released = true, scenario != "cleanup pending"
				if scenario == "successor" {
					objects = append(objects, &v1alpha3.MachineOperation{
						ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"},
						Spec: v1alpha3.MachineOperationSpec{
							OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name,
						},
						Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name}}},
					})
					wantAllowed = true
				}
			case "unclaimed IP":
				m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.40"
			case "no media claim":
				objects = []client.Object{m}
				wantAllowed = true
			case "unrelated PXE operation":
				old.Status.Targets[0].BootMedia = nil
				old.Status.Targets[0].MachineRef = "other-host"
				wantAllowed = true
			}

			fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(old).WithObjects(objects...).
				WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
			p.Client, p.Resolver.Reader = fc, fc

			if scenario == "active media" {
				owner, claimed, err := p.artifactOwner(t.Context(), media.Netboot.DHCPLeases[0].IPv4)
				require.NoError(t, err)
				require.True(t, claimed)
				require.Equal(t, old.UID, owner.op.UID)
				require.Equal(t, v1alpha3.PXEBootProtocolVirtualMedia, owner.status().Netboot.BootProtocol)
			}

			p.Resolver.Cluster = &StaticClusterInfo{}
			if scenario == "media client unavailable" {
				p.Client = nil
			}

			require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), map[string][]byte{
				"vmlinuz": []byte("live kernel"), "other-file": []byte("other content"),
			}))
			server := &TFTPServer{Media: p, FileResolver: *p.Resolver}

			for _, filename := range []string{"grub/grub.cfg", "/grub/grub.cfg", "vmlinuz", "other-file"} {
				t.Run(filename, func(t *testing.T) {
					var dst strings.Builder

					transfer := &tftpReaderFrom{readerFrom: readerFrom{Writer: &dst}, ip: m.Spec.Netboot().DHCPLeases[0].IPv4}

					err := server.readHandler(filename, transfer)
					if !wantAllowed {
						require.ErrorContains(t, err, "source IP is claimed by boot media")
						require.Empty(t, dst.String())

						return
					}

					require.NoError(t, err)
					require.NotEmpty(t, dst.String())

					if filename == "vmlinuz" {
						require.Equal(t, "live kernel", dst.String())
					}
				})
			}
		})
	}
}
