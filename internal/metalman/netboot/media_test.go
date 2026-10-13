// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

func TestMediaReleasedHistorySuccessorPrecedence(t *testing.T) {
	for _, protocol := range []string{v1alpha3.PXEBootProtocolPXE, v1alpha3.PXEBootProtocolHTTP} {
		t.Run(protocol, func(t *testing.T) {
			p, m, old := mediaFixture(t, "amd64")
			old.Status.Phase = v1alpha3.OperationPhaseComplete
			media := old.Status.Targets[0].BootMedia
			media.Cleaned, media.Released, media.ImageWritten = true, true, true
			m.Spec.Netboot().BootProtocol = protocol
			next := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name}, Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name}}}}
			fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(old).WithObjects(m, old).
				WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
			p.Client, p.Resolver.Reader = fc, fc
			require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), map[string][]byte{"vmlinuz": []byte("successor kernel")}))
			h := &HTTPServer{Media: p, FileResolver: *p.Resolver}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/vmlinuz", nil)
				r.RemoteAddr = "192.0.2.20:4321"
				w := httptest.NewRecorder()
				h.handleFile(w, r)

				return w
			}
			// Protocol edits alone cannot supersede a stale media install.
			require.Equal(t, http.StatusNotFound, request().Code)
			require.NoError(t, fc.Create(t.Context(), next))

			w := request()
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, "successor kernel", w.Body.String())
			// A current VirtualMedia Machine never falls through to PXE/HTTP.
			m.Spec.Netboot().BootProtocol = "VirtualMedia"
			require.NoError(t, fc.Update(t.Context(), m))
			require.Equal(t, http.StatusNotFound, request().Code)

			m.Spec.Netboot().BootProtocol = protocol
			require.NoError(t, fc.Update(t.Context(), m))
			// Cleanup still in progress takes precedence over the successor.
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(old), old))
			old.Status.Targets[0].BootMedia.Released = false
			require.NoError(t, fc.Status().Update(t.Context(), old))
			require.Equal(t, http.StatusNotFound, request().Code)
			// An active media owner keeps its frozen execution input even if a
			// non-media operation and mutable protocol claim the same lease.
			old.Status.Phase = ""
			old.Status.Targets[0].BootMedia.Cleaned = false
			old.Status.Targets[0].BootMedia.ImageWritten = false
			require.NoError(t, fc.Status().Update(t.Context(), old))
			o, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
			require.NoError(t, err)
			require.True(t, claimed)
			require.Equal(t, old.Name, o.op.Name)
		})
	}
}

func mediaFixture(t *testing.T, arch string) (*MediaProvider, *v1alpha3.Machine, *v1alpha3.MachineOperation) {
	t.Helper()

	ref := "test/netboot@sha256:" + strings.Repeat("a", 64)

	shim, grub := "shimx64.efi", "grubx64.efi"
	if arch == "arm64" {
		shim, grub = "shimaa64.efi", "grubaa64.efi"
	}

	tmpl, err := os.ReadFile("../../../images/netboot/assets/grub.cfg.tmpl")
	require.NoError(t, err)
	cache := NewOCICache(t.TempDir())
	require.NoError(t, populateOCICacheForArchitecture(cache.CacheDir, strings.Repeat("a", 64), arch, map[string][]byte{
		"metadata.yaml": []byte(fmt.Sprintf("mediaFormat: %s\nmediaShim: %s\nmediaGrub: %s\n", mediaFormat, shim, grub)),
		shim:            []byte("unchanged shim " + arch), grub: []byte("unchanged grub " + arch), "grub/grub.cfg.tmpl": tmpl,
	}))
	cache.SetDigestForArchitecture(ref, arch, "sha256:"+strings.Repeat("a", 64))
	m := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "host", UID: "host-uid"}, Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
		Image: "test/disk@sha256:" + strings.Repeat("b", 64), NetbootImage: ref, Architecture: arch, BootProtocol: "VirtualMedia",
		DHCPLeases: []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}},
	}}}}
	op := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "replace", UID: "op-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace}, Status: v1alpha3.MachineOperationStatus{
		Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name, BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
			ID: strings.Repeat("c", 64), MachineUID: m.UID, Netboot: m.Spec.Netboot().DeepCopy(), Phase: "Preparing",
		}}},
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(op).WithObjects(m, op).Build()
	p := &MediaProvider{Resolver: &FileResolver{Cache: cache, Reader: fc, ServeURL: "http://192.0.2.2:8080"}, Client: fc, CacheDir: cache.CacheDir, TFTPServer: "192.0.2.2"}

	return p, m, op
}

func mockMediaRunner(t *testing.T, calls *[]string) func(context.Context, string, ...string) error {
	t.Helper()

	return func(_ context.Context, name string, args ...string) error {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name == "xorriso" {
			for i, arg := range args {
				if arg == "-o" {
					return os.WriteFile(args[i+1], []byte("immutable ISO"), 0o600)
				}
			}
		}

		return nil
	}
}

func TestMediaPrepareAndRestart(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			p, machine, op := mediaFixture(t, arch)

			var calls []string

			p.Run = mockMediaRunner(t, &calls)
			id := op.Status.Targets[0].BootMedia.ID
			source, err := p.Prepare(t.Context(), machine, op, id)
			require.NoError(t, err)
			require.Equal(t, "ub-"+id+".iso", source.Name)
			require.Equal(t, "192.0.2.2", source.Server)

			payload := strings.Join(calls, "\n")
			if arch == "amd64" {
				require.Contains(t, payload, "shimx64.efi ::/EFI/BOOT/BOOTX64.EFI")
				require.Contains(t, payload, "grubx64.efi ::/EFI/BOOT/grubx64.efi")
			} else {
				require.Contains(t, payload, "shimaa64.efi ::/EFI/BOOT/BOOTAA64.EFI")
				require.Contains(t, payload, "grubaa64.efi ::/EFI/BOOT/grubaa64.efi")
			}

			require.Contains(t, payload, "-e efi.img -no-emul-boot")

			config, err := os.ReadFile(filepath.Join(p.CacheDir, "bootmedia", id, "grub.cfg"))
			require.NoError(t, err)

			for _, wanted := range []string{"net_add_addr ubprobe efinet0 0.0.0.0", "net_add_addr ubprobe efinet63 0.0.0.0", "$net_ubprobe_mac", "02:11:22:33:44:55", "net_add_addr ubnet $ubnic 192.0.2.20", "net_del_route ubnet:local", "net_add_route ublocal 192.0.2.0/24 ubnet", "net_add_route ubdefault 0.0.0.0/0 gw 192.0.2.1", "configfile (http,192.0.2.2:8080)/bootmedia/" + id + "/grub.cfg"} {
				require.Contains(t, string(config), wanted)
			}

			for _, forbidden := range []string{"dhcp", "unbounded.image_url", "password", "token", "chainloader"} {
				require.NotContains(t, string(config), forbidden)
			}

			count := len(calls)
			restarted := &MediaProvider{Resolver: p.Resolver, Client: p.Client, CacheDir: p.CacheDir, TFTPServer: p.TFTPServer, Run: p.Run}
			f, err := restarted.OpenISO(t.Context(), source.Name)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Len(t, calls, count)
			// A rack caller has no host-IP lookup at all on the media path.
			server := &TFTPServer{Media: restarted}

			var dst strings.Builder
			require.NoError(t, server.readHandler(source.Name, &readerFrom{Writer: &dst}))
			require.Equal(t, "immutable ISO", dst.String())

			for _, bad := range []string{"/" + source.Name, "../" + source.Name, "ub-unknown.iso", "ub-" + strings.Repeat("d", 64) + ".iso"} {
				_, err := restarted.OpenISO(t.Context(), bad)
				require.Error(t, err)
			}

			require.NoError(t, p.Release(t.Context(), id))
			_, err = p.Prepare(t.Context(), machine, op, id)
			require.NoError(t, err)
			require.Greater(t, len(calls), count)
		})
	}
}

type readerFrom struct{ io.Writer }

func (w *readerFrom) ReadFrom(r io.Reader) (int64, error) { return io.Copy(w.Writer, r) }

func TestMediaBuildFailureCleanup(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")
	p.Run = func(context.Context, string, ...string) error { return fmt.Errorf("tool failed") }
	_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.ErrorContains(t, err, "tool failed")
	entries, err := os.ReadDir(filepath.Join(p.CacheDir, "bootmedia"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMediaRejectInputs(t *testing.T) {
	for _, raw := range []string{"https://192.0.2.2", "http://host", "http://u:p@192.0.2.2", "http://192.0.2.2/?x", "http://192.0.2.2/#x", "http://192.0.2.2/a;halt", "http://192.0.2.2/%0a", "http://192.0.2.2:99999", "http://192.0.2.2/base"} {
		_, err := mediaServeURL(raw)
		require.Error(t, err, raw)
	}

	for _, mutate := range []func(*v1alpha3.PXESpec){
		func(s *v1alpha3.PXESpec) { s.DHCPLeases = nil },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].Gateway = "" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].Gateway = "198.51.100.1" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].IPv4 = "192.0.2.20;halt" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].MAC = "02:11:22:33:44:55\nhalt" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].SubnetMask = "255.0.255.0" },
		func(s *v1alpha3.PXESpec) { s.TargetDisk = "/dev/sda;halt" },
	} {
		_, m, _ := mediaFixture(t, "amd64")
		mutate(m.Spec.Netboot())
		require.Error(t, validateMediaLease(m.Spec.Netboot()))
	}

	p, m, op := mediaFixture(t, "amd64")
	op.Status.Targets[0].BootMedia.Netboot.Image = "test/disk:mutable"
	m.Spec.Netboot().Image = "test/disk:mutable"
	require.NoError(t, p.Client.Update(t.Context(), m))
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.ErrorContains(t, err, "digest-pinned")
}

func mediaRequest(p *MediaProvider, id, method, action, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/bootmedia/"+id+"/"+action, nil)
	r.RemoteAddr = ip + ":4321"
	r.Header.Set("X-Forwarded-For", "192.0.2.20")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)

	return w
}

func TestMediaScopedCallbacks(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: "other-host", BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ID: strings.Repeat("d", 64)}})
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	id := op.Status.Targets[0].BootMedia.ID
	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "linux (http,192.0.2.2:8080)/vmlinuz")
	require.Contains(t, w.Body.String(), "initrd (http,192.0.2.2:8080)/initrd (http,192.0.2.2:8080)/init.cpio")
	require.Contains(t, w.Body.String(), "unbounded.virtual_media=1")
	require.Contains(t, w.Body.String(), "unbounded.media_callback=http://192.0.2.2:8080/bootmedia/"+id)
	require.NotContains(t, w.Body.String(), "chainloader")
	require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.99").Code)
	require.Equal(t, http.StatusMethodNotAllowed, mediaRequest(p, id, http.MethodGet, "complete", "192.0.2.20").Code)
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)

	for range 2 {
		require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.20").Code)
	}

	var current v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.True(t, current.Status.Targets[0].BootMedia.ImageWritten)
	require.False(t, current.Status.Targets[1].BootMedia.ImageWritten)
	require.Empty(t, current.Status.Conditions)
	require.Equal(t, "halt\n", mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Body.String())
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)

	current.Status.Targets[0].BootMedia.Cleaned = true
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.NotNil(t, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	ack := current.Status.Targets[0].BootMedia.RebootRequestedAt.DeepCopy()

	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.Equal(t, ack, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	_, err := p.OpenISO(t.Context(), "ub-"+id+".iso")
	require.Error(t, err)

	current.Status.Phase = v1alpha3.OperationPhaseComplete
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
}

func TestMediaRejectPathAliasesAndArtifactDrift(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")

	id := op.Status.Targets[0].BootMedia.ID
	for _, path := range []string{"/bootmedia/" + id + "/../grub.cfg", "/bootmedia//" + id + "/grub.cfg", "/bootmedia/%63" + id[1:] + "/grub.cfg"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "192.0.2.20:4321"
		w := httptest.NewRecorder()
		normalizePath(p).ServeHTTP(w, r)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	_, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, _ = p.artifactOwner(t.Context(), "192.0.2.21")
	require.False(t, claimed)

	m.Spec.Netboot().Image = "other/disk@sha256:" + strings.Repeat("e", 64)
	require.NoError(t, p.Client.Update(t.Context(), m))
	o, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, op.Status.Targets[0].BootMedia.Netboot.Image, o.machine.Spec.Netboot().Image)
}

func TestMediaCallbackPersistenceFailure(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	p.Client = interceptor.NewClient(p.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return fmt.Errorf("conflict")
	}})
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, op.Status.Targets[0].BootMedia.ID, http.MethodPost, "complete", "192.0.2.20").Code)
}

func TestMediaCallbacksOutliveInstallAuthorization(t *testing.T) {
	for _, state := range []string{"operation-failed", "target-failed", "operation-deleting", "machine-deleted", "machine-replaced"} {
		t.Run(state, func(t *testing.T) {
			p, m, op := mediaFixture(t, "amd64")
			id := op.Status.Targets[0].BootMedia.ID

			switch state {
			case "operation-failed":
				op.Status.Phase = v1alpha3.OperationPhaseFailed
			case "target-failed":
				op.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
			case "operation-deleting":
				op.Finalizers = []string{"test.example/cleanup"}
				require.NoError(t, p.Client.Update(t.Context(), op))
				require.NoError(t, p.Client.Delete(t.Context(), op))
				require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			case "machine-deleted", "machine-replaced":
				require.NoError(t, p.Client.Delete(t.Context(), m))

				if state == "machine-replaced" {
					m.UID, m.ResourceVersion = "replacement", ""
					require.NoError(t, p.Client.Create(t.Context(), m))
				}
			}

			require.NoError(t, p.Client.Status().Update(t.Context(), op))
			require.Equal(t, "halt\n", mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Body.String())
			_, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
			require.True(t, claimed)
			require.Error(t, err)
			require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.21").Code)
			require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.20").Code)
			require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
			require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			op.Status.Targets[0].BootMedia.Cleaned = true
			require.NoError(t, p.Client.Status().Update(t.Context(), op))
			require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
			require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			require.NotNil(t, op.Status.Targets[0].BootMedia.RebootRequestedAt)
		})
	}
}

func TestMediaReadyPersistenceFailure(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	op.Status.Targets[0].BootMedia.Cleaned = true
	op.Status.Targets[0].BootMedia.ImageWritten = true
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	p.Client = interceptor.NewClient(p.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return fmt.Errorf("conflict")
	}})
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, op.Status.Targets[0].BootMedia.ID, http.MethodGet, "ready", "192.0.2.20").Code)
}

func TestMediaInstallerWaitsThroughLateCleanup(t *testing.T) {
	content, err := os.ReadFile("../../../images/netboot/assets/init")
	require.NoError(t, err)

	_, barrier, found := strings.Cut(string(content), "if [ \"$VIRTUAL_MEDIA\" = 1 ]; then\n    # Stay in RAM")
	require.True(t, found)
	barrier, _, found = strings.Cut(barrier, "elif [ -n \"$SERVE_URL\" ]; then")
	require.True(t, found)
	// Execute just the handoff block. Mock network and sleep, never disk writes
	// or reboot. Delayed acknowledgment must resume from the same barrier.
	dir := t.TempDir()
	script := "set -eu\ncd '" + dir + "'\nMEDIA_CALLBACK=http://192.0.2.2/bootmedia/test\n" + `
log() { :; }
sleep() { :; }
curl() {
    for arg in "$@"; do last="$arg"; done
    case "$last" in
        */complete) marker=complete ;;
        */ready) marker=ready ;;
        *) exit 9 ;;
    esac
    if [ ! -f "$marker" ]; then
        touch "$marker"
        printf 503
    else
        printf 200
    fi
}
` + "# Stay in RAM" + barrier + "\n[ -f complete ] && [ -f ready ]\n"
	path := filepath.Join(dir, "barrier.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o600))
	require.NoError(t, runMediaCommand(t.Context(), "sh", path))
	require.Contains(t, barrier, "--max-time 15")
	require.Contains(t, barrier, `[ "$status" = 200 ]`)
	require.NotContains(t, barrier, "fatal")
	require.NotContains(t, barrier, "write_image")
}

func TestMediaFrozenArtifactsAfterPrepare(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")

	var calls []string

	p.Run = mockMediaRunner(t, &calls)
	id := op.Status.Targets[0].BootMedia.ID
	_, err := p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)

	frozen := op.Status.Targets[0].BootMedia.Netboot
	p.Resolver.Cache.SetDigestForArchitecture(frozen.Image, "amd64", "sha256:"+strings.Repeat("b", 64))
	require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("b", 64), map[string][]byte{"disk.img.gz": []byte("frozen disk")}))

	m.Spec.Netboot().Image = "changed/image:latest"
	m.Spec.Netboot().NetbootImage = "changed/boot:latest"
	m.Spec.Netboot().DHCPLeases[0].IPv4 = "198.51.100.20"
	m.Spec.Netboot().TargetDisk = "/dev/changed"
	m.Spec.Netboot().BootProtocol = "PXE"
	m.Status.Redfish = &v1alpha3.RedfishStatus{CertFingerprint: "changed-pin"}
	require.NoError(t, p.Client.Update(t.Context(), m))
	_, err = p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)

	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "ip=192.0.2.20::192.0.2.1")
	require.NotContains(t, w.Body.String(), "/dev/changed")

	h := &HTTPServer{Media: p, FileResolver: *p.Resolver}
	r := httptest.NewRequest(http.MethodGet, "/disk.img.gz", nil)
	r.RemoteAddr = "192.0.2.20:4321"
	w = httptest.NewRecorder()
	h.handleFile(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "frozen disk", w.Body.String())

	op.Status.Phase = v1alpha3.OperationPhaseFailed
	require.NoError(t, p.Client.Status().Update(t.Context(), op))

	w = httptest.NewRecorder()
	h.handleFile(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMediaIdentityAndOldImage(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		p, m, op := mediaFixture(t, "amd64")

		var calls []string

		p.Run = mockMediaRunner(t, &calls)
		m.Spec.Netboot().TargetDisk = "/dev/sdb"
		require.NoError(t, p.Client.Update(t.Context(), m))
		_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.NoError(t, err)
		require.NoError(t, p.Client.Delete(t.Context(), m))
		m.ResourceVersion = ""
		m.UID = "replacement-uid"
		require.NoError(t, p.Client.Create(t.Context(), m))
		_, err = p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "identity")
	})
	t.Run("old image", func(t *testing.T) {
		p, m, op := mediaFixture(t, "amd64")
		meta, err := p.Resolver.Cache.MetadataForRefArchitecture(m.Spec.Netboot().NetbootImage, "amd64")
		require.NoError(t, err)

		meta.MediaFormat = ""
		_, err = p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "lacks compatible")
	})
}

func TestMediaNoInstallHalts(t *testing.T) {
	tmpl, err := os.ReadFile("../../../images/netboot/assets/grub.cfg.tmpl")
	require.NoError(t, err)
	data, err := renderTemplate(string(tmpl), templateData{VirtualMedia: true})
	require.NoError(t, err)
	require.Contains(t, string(data), "halt")
	require.NotContains(t, string(data), "chainloader")
	require.NotContains(t, string(data), "linux")
}

func TestMediaRealToolsOptional(t *testing.T) {
	for _, tool := range []string{"mkfs.vfat", "mmd", "mcopy", "xorriso"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("ISO packaging tool not installed: " + tool)
		}
	}

	p, m, op := mediaFixture(t, "amd64")
	source, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.NoError(t, err)
	f, err := p.OpenISO(t.Context(), source.Name)
	require.NoError(t, err)

	defer f.Close()

	stat, err := f.Stat()
	require.NoError(t, err)
	require.Greater(t, stat.Size(), int64(16<<20))

	first, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	id := op.Status.Targets[0].BootMedia.ID
	require.NoError(t, p.Release(t.Context(), id))
	_, err = p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)
	second, err := os.ReadFile(filepath.Join(p.CacheDir, "bootmedia", id, "bootstrap.iso"))
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(first), sha256.Sum256(second), "regeneration must preserve ISO bytes")
	// Inspect the FAT payload, not only command arguments.
	for _, name := range []string{"BOOTX64.EFI", "grubx64.efi", "grub.cfg"} {
		output := filepath.Join(t.TempDir(), name)
		require.NoError(t, runMediaCommand(t.Context(), "mcopy", "-i", filepath.Join(p.CacheDir, "bootmedia", id, "root", "efi.img"), "::/EFI/BOOT/"+name, output))
		content, err := os.ReadFile(output)
		require.NoError(t, err)

		if name == "BOOTX64.EFI" {
			require.Equal(t, "unchanged shim amd64", string(content))
		}

		if name == "grubx64.efi" {
			require.Equal(t, "unchanged grub amd64", string(content))
		}

		if name == "grub.cfg" {
			require.Contains(t, string(content), "/bootmedia/"+id+"/grub.cfg")
		}
	}
}
