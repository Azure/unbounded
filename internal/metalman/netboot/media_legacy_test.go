// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

func TestMediaLegacyServingAfterLiveLeaseChange(t *testing.T) {
	for _, protocol := range []string{v1alpha3.PXEBootProtocolPXE, v1alpha3.PXEBootProtocolHTTP} {
		for _, scenario := range []string{
			"installing", "cleanup pending", "abandoned but unreleased", "released but active",
			"active with successor", "released successor", "other site", "empty media site",
			"empty provider site", "both sites empty", "different UID",
		} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				p, m, op := mediaFixture(t, "amd64")
				m.Spec.Netboot().BootProtocol = protocol
				m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.40"
				b := op.Status.Targets[0].BootMedia
				b.Phase = "Installing"
				objects := []client.Object{m, op}
				wantAllowed := false

				switch scenario {
				case "cleanup pending":
					op.Status.Phase = v1alpha3.OperationPhaseFailed
					b.Cleaned = true
				case "abandoned but unreleased":
					b.Abandoned = true
				case "released but active":
					b.Released, b.Cleaned = true, true
				case "released successor":
					op.Status.Phase = v1alpha3.OperationPhaseComplete
					b.Released, b.Cleaned = true, true
					wantAllowed = true
				case "other site":
					b.Site = "site-b"
					wantAllowed = true
				case "empty media site":
					b.Site = ""
					wantAllowed = true
				case "empty provider site":
					p.Site, m.Labels["unbounded-cloud.io/site"] = "", ""
					wantAllowed = true
				case "both sites empty":
					p.Site, b.Site, m.Labels["unbounded-cloud.io/site"] = "", "", ""
				case "different UID":
					m.UID = "replacement-uid"
					wantAllowed = true
				}

				if scenario == "released successor" || scenario == "active with successor" {
					objects = append(objects, &v1alpha3.MachineOperation{
						ObjectMeta: metav1.ObjectMeta{Name: "successor"},
						Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name},
					})
				}

				fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objects...).
					WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
				p.Client, p.Resolver.Reader = fc, fc
				p.Resolver.Cluster = &StaticClusterInfo{}
				require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), map[string][]byte{
					"vmlinuz":            []byte("live kernel"),
					"grub.cfg.tmpl":      []byte("install={{ .InstallRequested }}"),
					"grub/grub.cfg.tmpl": []byte("install={{ .InstallRequested }}"),
				}))
				h := &HTTPServer{
					Media:          p,
					FileResolver:   *p.Resolver,
					TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.30/32")},
				}
				tftp := &TFTPServer{Media: p, FileResolver: *p.Resolver}
				ip := m.Spec.Netboot().DHCPLeases[0].IPv4

				for _, path := range []string{"vmlinuz", "grub.cfg", "grub/grub.cfg"} {
					t.Run("HTTP/"+path, func(t *testing.T) {
						for _, source := range []string{ip, "192.0.2.30"} {
							forwarded := ""
							if source != ip {
								forwarded = ip
							}

							w := firstBootRequest(h, "/"+path, source, "", forwarded)
							if wantAllowed {
								require.Equal(t, http.StatusOK, w.Code, w.Body.String())

								if scenario == "released successor" && strings.HasSuffix(path, "grub.cfg") {
									require.Equal(t, "install=true", w.Body.String())
								}
							} else {
								require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
							}
						}
					})
					t.Run("TFTP/"+path, func(t *testing.T) {
						var dst strings.Builder

						transfer := &tftpReaderFrom{readerFrom: readerFrom{Writer: &dst}, ip: ip}

						err := tftp.readHandler(path, transfer)
						if wantAllowed {
							require.NoError(t, err)
							require.NotEmpty(t, dst.String())

							if scenario == "released successor" && strings.HasSuffix(path, "grub.cfg") {
								require.Equal(t, "install=true", dst.String())
							}
						} else {
							require.ErrorContains(t, err, "source IP is claimed by boot media")
							require.Empty(t, dst.String())
						}
					})
				}
			})
		}
	}
}

func TestInstallRequestedExcludesVirtualMedia(t *testing.T) {
	for _, scenario := range []string{"retained target", "live media without targets", "live media with target", "legacy successor"} {
		t.Run(scenario, func(t *testing.T) {
			p, m, op := mediaFixture(t, "amd64")
			op.Spec.MachineRef = m.Name
			objects := []client.Object{m, op}

			switch scenario {
			case "retained target", "legacy successor":
				m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
			case "live media without targets":
				op.Status.Targets = nil
			case "live media with target":
				op.Status.Targets[0].BootMedia = nil
			}

			if scenario == "legacy successor" {
				objects = append(objects, &v1alpha3.MachineOperation{
					ObjectMeta: metav1.ObjectMeta{Name: "successor"},
					Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name},
				})
			}

			p.Resolver.Reader = fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objects...).Build()
			requested, err := p.Resolver.installRequested(t.Context(), m)
			require.NoError(t, err)
			require.Equal(t, scenario == "legacy successor", requested)
		})
	}
}
