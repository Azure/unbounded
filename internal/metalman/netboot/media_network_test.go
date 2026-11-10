// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/hostnetwork"
)

func testHostNetwork() *v1alpha3.HostNetworkSpec {
	return &v1alpha3.HostNetworkSpec{
		Ethernets: []v1alpha3.EthernetSpec{
			{Name: "enp1s0f0", MACAddress: "02:11:22:33:44:55"},
			{Name: "enp1s0f1", MACAddress: "02:00:00:00:00:02"},
		},
		Bonds: []v1alpha3.BondSpec{{
			Name:       "bond0",
			Interfaces: []string{"enp1s0f0", "enp1s0f1"},
			InterfaceAddressing: v1alpha3.InterfaceAddressing{
				Addresses: []string{"192.0.2.20/24"},
				Gateway:   "192.0.2.1",
			},
		}},
		VLANs: []v1alpha3.VLANSpec{{
			Name: "bond0.64", ID: 64, Link: "bond0",
			InterfaceAddressing: v1alpha3.InterfaceAddressing{Addresses: []string{"198.51.100.20/24"}},
		}},
		DNS: []string{"192.0.2.53"},
	}
}

// networkMediaFixture is mediaFixture with a declared host network and a
// netboot image that advertises the local installer.
func networkMediaFixture(t *testing.T, arch, meta string) (*MediaProvider, *v1alpha3.Machine, *v1alpha3.MachineOperation) {
	t.Helper()

	p, m, op := mediaFixture(t, arch)
	require.NoError(t, populateOCICacheForArchitecture(p.CacheDir, strings.Repeat("a", 64), arch, map[string][]byte{
		"metadata.yaml": []byte(meta),
		"vmlinuz":       []byte("kernel " + arch),
		"initrd":        []byte("initrd " + arch),
		"init.cpio":     []byte("init " + arch),
	}))

	m.Spec.Netboot().Network = testHostNetwork()
	m.Spec.Netboot().TargetDisk = "/dev/disk/by-id/test-os-disk"
	require.NoError(t, p.Client.Update(t.Context(), m))

	op.Status.Targets[0].BootMedia.Netboot = m.Spec.Netboot().DeepCopy()
	require.NoError(t, p.Client.Status().Update(t.Context(), op))

	return p, m, op
}

const localMeta = "mediaFormat: " + mediaFormat + "\nlocalInstaller: " + localInstallerFormat + "\n"

func TestMediaLocalInstallerISO(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			p, m, op := networkMediaFixture(t, arch, localMeta)
			id := op.Status.Targets[0].BootMedia.ID

			var (
				calls  []string
				config []byte
				tree   = map[string][]byte{}
			)

			run := mockMediaRunner(t, &calls)
			p.Run = func(ctx context.Context, name string, args ...string) error {
				if name == "mcopy" && args[len(args)-1] == "::/EFI/BOOT/grub.cfg" {
					var err error

					config, err = os.ReadFile(args[len(args)-2])
					require.NoError(t, err)
				}

				if name == "xorriso" {
					root := args[len(args)-1]
					require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
						require.NoError(t, err)

						if d.IsDir() {
							return nil
						}

						rel, err := filepath.Rel(root, path)
						require.NoError(t, err)
						data, err := os.ReadFile(path)
						require.NoError(t, err)

						tree[filepath.ToSlash(rel)] = data

						return nil
					}))
				}

				return run(ctx, name, args...)
			}

			_, err := p.Prepare(t.Context(), m, op, id)
			require.NoError(t, err)

			wantCPIO, err := hostnetwork.MediaCPIO(testHostNetwork(), m.Spec.Netboot().DHCPLeases[0])
			require.NoError(t, err)
			require.Equal(t, map[string][]byte{
				"unbounded/" + id:      {},
				"unbounded/vmlinuz":    []byte("kernel " + arch),
				"unbounded/initrd":     []byte("initrd " + arch),
				"unbounded/init.cpio":  []byte("init " + arch),
				"unbounded/media.cpio": wantCPIO,
			}, tree)

			console := " console=tty0 console=ttyS0,115200n8"
			if arch == "arm64" {
				console = ""
			}

			require.Equal(t, "search --no-floppy --file --set=root /unbounded/"+id+"\n"+
				"linux /unbounded/vmlinuz unbounded.install_url=http://192.0.2.2:8080/bootmedia/"+id+console+"\n"+
				"initrd /unbounded/initrd /unbounded/init.cpio /unbounded/media.cpio\n"+
				"boot\n", string(config))
			require.NotContains(t, string(config), "efinet")

			// The identity uses the local format, so it differs from legacy media.
			identity, err := os.ReadFile(filepath.Join(p.CacheDir, "bootmedia", id, "identity"))
			require.NoError(t, err)

			legacy, lm, lop := mediaFixture(t, arch)
			legacy.Run = mockMediaRunner(t, &calls)
			_, err = legacy.Prepare(t.Context(), lm, lop, id)
			require.NoError(t, err)
			legacyIdentity, err := os.ReadFile(filepath.Join(legacy.CacheDir, "bootmedia", id, "identity"))
			require.NoError(t, err)
			require.NotEqual(t, string(legacyIdentity), string(identity))
		})
	}
}

func TestMediaLocalInstallerRejectsInputs(t *testing.T) {
	t.Run("missing localInstaller", func(t *testing.T) {
		p, m, op := networkMediaFixture(t, "amd64", "mediaFormat: "+mediaFormat+"\n")

		var calls []string

		p.Run = mockMediaRunner(t, &calls)
		_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, localInstallerFormat)
		require.Empty(t, calls)
	})

	t.Run("https serve URL", func(t *testing.T) {
		p, m, op := networkMediaFixture(t, "amd64", localMeta)
		p.Resolver.ServeURL = "https://192.0.2.2:8443"

		var calls []string

		p.Run = mockMediaRunner(t, &calls)
		_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "plain HTTP")
		require.Empty(t, calls)
	})

	t.Run("invalid network", func(t *testing.T) {
		p, m, op := networkMediaFixture(t, "amd64", localMeta)
		// The lease address is not declared on any interface.
		op.Status.Targets[0].BootMedia.Netboot.Network.Bonds[0].Addresses = []string{"192.0.2.21/24"}
		require.NoError(t, p.Client.Status().Update(t.Context(), op))

		var calls []string

		p.Run = mockMediaRunner(t, &calls)
		_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "invalid host network")
		require.Empty(t, calls)
	})
}

func TestMediaInstallEndpoint(t *testing.T) {
	p, _, op := networkMediaFixture(t, "amd64", localMeta)
	p.Resolver.Cluster = &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://203.0.113.5:6443"}}
	id := op.Status.Targets[0].BootMedia.ID

	// Before power-on the host is not expected to boot.
	require.Equal(t, http.StatusConflict, mediaRequest(p, id, http.MethodGet, "install", "192.0.2.20").Code)

	setMediaPhase(t, p, op, "PoweringOn")
	require.Equal(t, http.StatusMethodNotAllowed, mediaRequest(p, id, http.MethodPost, "install", "192.0.2.20").Code)
	require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodGet, "install", "192.0.2.99").Code)
	require.Equal(t, http.StatusNotFound, mediaRequest(p, strings.Repeat("d", 32), http.MethodGet, "install", "192.0.2.20").Code)
	// Network media never serves a network installer GRUB config.
	require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Code)

	var current v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.Nil(t, current.Status.Targets[0].BootMedia.LoaderStartedAt)

	w := mediaRequest(p, id, http.MethodGet, "install", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "text/plain", w.Header().Get("Content-Type"))
	require.Equal(t, "IMAGE_URL=http://192.0.2.2:8080/disk.img.gz\n"+
		"SERVE_URL=http://192.0.2.2:8080\n"+
		"DS_URL=http://192.0.2.2:8080/cloud-init/\n"+
		"NODE_NAME=host\n"+
		"NODE_NAMESPACE=\n"+
		"APISERVER_URL=https://203.0.113.5:6443\n"+
		"MEDIA_CALLBACK=http://192.0.2.2:8080/bootmedia/"+id+"\n"+
		"DISK=/dev/disk/by-id/test-os-disk\n", w.Body.String())

	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	started := current.Status.Targets[0].BootMedia.LoaderStartedAt
	require.NotNil(t, started)
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "install", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.Equal(t, started, current.Status.Targets[0].BootMedia.LoaderStartedAt)

	// The existing callbacks work for network media.
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.20").Code)
	// Once the image is written, install is no longer authorized.
	require.Equal(t, http.StatusConflict, mediaRequest(p, id, http.MethodGet, "install", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	current.Status.Targets[0].BootMedia.Cleaned = true
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
}

func TestMediaInstallEndpointRejectsUnsafeValues(t *testing.T) {
	p, _, op := networkMediaFixture(t, "amd64", localMeta)
	p.Resolver.Cluster = &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://203.0.113.5:6443/a b"}}
	setMediaPhase(t, p, op, "Installing")
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, op.Status.Targets[0].BootMedia.ID, http.MethodGet, "install", "192.0.2.20").Code)
}

func TestMediaInstallEndpointLegacyMedia(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	setMediaPhase(t, p, op, "Installing")
	id := op.Status.Targets[0].BootMedia.ID
	require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodGet, "install", "192.0.2.20").Code)

	var current v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.Nil(t, current.Status.Targets[0].BootMedia.LoaderStartedAt)

	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "linux (http,192.0.2.2:8080)/vmlinuz")
}

func TestNetworkConfigTemplate_HostNetplan(t *testing.T) {
	networkTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "network-config.tmpl"))
	require.NoError(t, err)

	node := &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
		DHCPLeases: []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}},
		Network:    testHostNetwork(),
	}}}

	data, err := newHostTemplateData(node, ClusterInfo{}, "", "", "192.0.2.20", false)
	require.NoError(t, err)

	want, err := hostnetwork.RenderNetplanConfig(testHostNetwork())
	require.NoError(t, err)
	require.Equal(t, strings.TrimRight(string(want), "\n"), data.HostNetplan)

	result, err := renderTemplate(string(networkTmpl), data)
	require.NoError(t, err)
	require.Contains(t, string(result), "bond0")
	require.Contains(t, string(result), "bond0.64")
	require.NotContains(t, string(result), "provisioning:")
	require.True(t, strings.HasPrefix(strings.TrimLeft(string(result), "\n"), "version: 2"))

	// A network that cannot be rendered fails rather than rendering nothing.
	node.Spec.PXE.Network.VLANs[0].Link = "missing"
	_, err = newHostTemplateData(node, ClusterInfo{}, "", "", "192.0.2.20", false)
	require.Error(t, err)

	// Without a network, the legacy data is unchanged.
	node.Spec.PXE.Network = nil
	data, err = newHostTemplateData(node, ClusterInfo{}, "", "", "192.0.2.20", false)
	require.NoError(t, err)
	require.Equal(t, newTemplateData(node, ClusterInfo{}, "", "", "192.0.2.20", false).values(), data.values())
}
