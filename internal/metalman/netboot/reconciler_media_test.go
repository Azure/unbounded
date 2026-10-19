// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"oras.land/oras-go/v2/content/oci"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestOCIReconcilerMapOperationToImage(t *testing.T) {
	spec := &v1alpha3.PXESpec{
		Image: "example.com/disk:v1", NetbootImage: "example.com/netboot:v1",
		Architecture:  v1alpha3.PXEArchitectureARM64,
		PullSecretRef: secretRef("frozen", "disk"), NetbootPullSecretRef: secretRef("frozen", "netboot"),
	}
	op := &v1alpha3.MachineOperation{
		Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace},
		Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{
			BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Netboot: spec, Site: "rack-a"},
		}}},
	}
	r := &OCIReconciler{Site: "rack-a", DefaultNetbootRef: "example.com/default:v2"}

	tests := []struct {
		name string
		edit func(*v1alpha3.MachineOperation)
		want int
	}{
		{name: "active", want: 2},
		{name: "terminal but retained", edit: func(op *v1alpha3.MachineOperation) { op.Status.Phase = v1alpha3.OperationPhaseFailed }, want: 2},
		{name: "deleting but retained", edit: func(op *v1alpha3.MachineOperation) { now := metav1.Now(); op.DeletionTimestamp = &now }, want: 2},
		{name: "cleaned but retained", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia.Cleaned = true }, want: 2},
		{name: "released but not cleaned", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia.Released = true }, want: 2},
		{name: "released but not terminal", edit: func(op *v1alpha3.MachineOperation) {
			op.Status.Targets[0].BootMedia.Released = true
			op.Status.Targets[0].BootMedia.Cleaned = true
		}, want: 2},
		{name: "released terminal operation", edit: func(op *v1alpha3.MachineOperation) {
			op.Status.Targets[0].BootMedia.Released = true
			op.Status.Targets[0].BootMedia.Cleaned = true
			op.Status.Phase = v1alpha3.OperationPhaseComplete
		}},
		{name: "released terminal target", edit: func(op *v1alpha3.MachineOperation) {
			op.Status.Targets[0].BootMedia.Released = true
			op.Status.Targets[0].BootMedia.Cleaned = true
			op.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
		}},
		{name: "other site", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia.Site = "rack-b" }},
		{name: "no media", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia = nil }},
		{name: "no snapshot", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia.Netboot = nil }},
		{name: "other operation kind", edit: func(op *v1alpha3.MachineOperation) { op.Spec.OperationKind = "" }},
		{name: "no default for frozen netboot", edit: func(op *v1alpha3.MachineOperation) { op.Status.Targets[0].BootMedia.Netboot.NetbootImage = "" }, want: 1},
		{name: "deduplicate targets", edit: func(op *v1alpha3.MachineOperation) {
			op.Status.Targets = append(op.Status.Targets, op.Status.Targets[0])
		}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := op.DeepCopy()
			if tt.edit != nil {
				tt.edit(current)
			}

			reqs := r.mapOperationToImage(t.Context(), current)
			require.Len(t, reqs, tt.want)

			for i, req := range reqs {
				got, err := decodeImagePullRequest(req.NamespacedName)
				require.NoError(t, err)

				want := imagePullRequest{ImageRef: spec.Image, Architecture: spec.TargetArchitecture(), PullSecretRef: spec.PullSecretRef}
				if i == 1 {
					want.ImageRef, want.PullSecretRef = spec.NetbootImage, spec.NetbootPullSecretRef
				}

				require.Equal(t, want, got)
			}
		})
	}

	require.Empty(t, r.mapOperationToImage(t.Context(), &v1alpha3.Machine{}))
	// Both watches must produce the same queue identity for shared inputs.
	r.DefaultNetbootRef = ""
	machine := &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: spec}}}
	require.Equal(t, r.mapMachineToImage(t.Context(), machine), r.mapOperationToImage(t.Context(), op))
}

func TestOCIReconcilerRestoresFrozenMediaAfterRestart(t *testing.T) {
	tests := []struct {
		name          string
		missingSecret bool
		released      bool
		edit          func(*v1alpha3.MachineOperationTargetStatus)
		wantDisk      bool
		wantBoot      bool
	}{
		{name: "missing-secret=false", wantDisk: true, wantBoot: true},
		{name: "missing-secret=true", missingSecret: true, wantDisk: true, wantBoot: true},
		{name: "released first boot pending", released: true, wantBoot: true},
		{name: "released first boot running", released: true, wantBoot: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) {
			target.Conditions = []metav1.Condition{{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionFalse, Reason: "Running"}}
		}},
		{name: "released first boot succeeded", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) {
			target.Conditions = []metav1.Condition{{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue, Reason: "Succeeded"}}
		}},
		{name: "released first boot failed", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) {
			target.Conditions = []metav1.Condition{{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionFalse, Reason: "Failed"}}
		}},
		{name: "released abandoned", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) { target.BootMedia.Abandoned = true }},
		{name: "released image not written", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) { target.BootMedia.ImageWritten = false }},
		{name: "released reboot not requested", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) { target.BootMedia.RebootRequestedAt = nil }},
		{name: "released no lease", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) { target.BootMedia.Netboot.DHCPLeases = nil }},
		{name: "released other site", released: true, edit: func(target *v1alpha3.MachineOperationTargetStatus) { target.BootMedia.Site = "rack-b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			diskStore, diskDigest := pushTestImage(t, []byte("frozen disk"))
			bootStore, bootDigest := pushTestImage(t, []byte("frozen netboot"))
			liveStore, liveDigest := pushTestImage(t, []byte("edited images"))
			stores := map[string]*oci.Store{diskDigest: diskStore, bootDigest: bootStore, liveDigest: liveStore}

			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer test-token" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.invalid/token",service="test"`)
					w.WriteHeader(http.StatusUnauthorized)

					return
				}
				// Only digest resolution is needed when unpacked files survive a restart.
				if req.Method != http.MethodHead {
					t.Errorf("unexpected registry fetch: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)

					return
				}

				digest := filepath.Base(req.URL.Path)

				store := stores[digest]
				if store == nil {
					http.NotFound(w, req)
					return
				}

				desc, err := store.Resolve(req.Context(), digest)
				if err != nil {
					t.Errorf("resolve test manifest: %v", err)
					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				w.Header().Set("Content-Type", desc.MediaType)
				w.Header().Set("Content-Length", fmt.Sprint(desc.Size))
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusOK)
			}))
			defer registry.Close()

			host := strings.TrimPrefix(registry.URL, "http://")
			secret := dockerConfigJSONSecret("frozen", fmt.Sprintf(`{"auths":{%q:{"registrytoken":"test-token"}}}`, host))
			secret.Namespace = "images"
			liveSecret := secret.DeepCopy()
			liveSecret.Name = "live"
			spec := &v1alpha3.PXESpec{
				Image: host + "/disk@" + diskDigest, NetbootImage: host + "/netboot@" + bootDigest,
				Architecture:  v1alpha3.PXEArchitectureARM64,
				PullSecretRef: secretRef(secret.Namespace, secret.Name), NetbootPullSecretRef: secretRef(secret.Namespace, secret.Name),
				DHCPLeases: []v1alpha3.DHCPLease{{IPv4: "192.0.2.10", MAC: "02:00:00:00:00:01", Gateway: "192.0.2.1", SubnetMask: "255.255.255.0"}},
			}
			machine := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid", Labels: map[string]string{"unbounded-cloud.io/site": "rack-a"}},
				Spec:       v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: spec.DeepCopy()}},
			}
			op := &v1alpha3.MachineOperation{
				ObjectMeta: metav1.ObjectMeta{Name: "install"},
				Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace},
				Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{
					MachineRef: machine.Name, BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Netboot: spec.DeepCopy(), Site: "rack-a", MachineUID: machine.UID},
				}}},
			}

			if tt.released {
				now := metav1.Now()
				target := &op.Status.Targets[0]
				target.Phase = v1alpha3.OperationPhaseFailed
				target.BootMedia.Released = true
				target.BootMedia.Cleaned = true
				target.BootMedia.ImageWritten = true
				target.BootMedia.RebootRequestedAt = &now
			}

			if tt.edit != nil {
				tt.edit(&op.Status.Targets[0])
			}

			frozenSpec := op.Status.Targets[0].BootMedia.Netboot.DeepCopy()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, v1alpha3.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, op, secret, liveSecret).Build()

			cache := NewOCICache(t.TempDir())
			for _, image := range []struct{ digest, arch, payload string }{
				{diskDigest, "arm64", "frozen disk"}, {bootDigest, "arm64", "frozen netboot"}, {liveDigest, "amd64", "edited images"},
			} {
				dir := cache.DiskDirForArchitecture(image.digest, image.arch)
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "disk.img"), []byte(image.payload), 0o600))
			}

			cloudInitDir := filepath.Join(cache.DiskDirForArchitecture(bootDigest, "arm64"), "cloud-init")
			require.NoError(t, os.MkdirAll(cloudInitDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(cloudInitDir, "vendor-data.tmpl"), []byte("frozen cloud-init {{ .Machine.Name }}"), 0o600))

			r := &OCIReconciler{Client: c, Cache: cache, Site: "rack-a"}
			for _, req := range r.mapMachineToImage(ctx, machine) {
				result, err := r.Reconcile(ctx, req)
				require.NoError(t, err)
				require.Equal(t, imageResyncInterval, result.RequeueAfter)
			}

			require.Equal(t, diskDigest, cache.DigestForArchitecture(spec.Image, "arm64"))
			machine.Spec.Host.Netboot = &v1alpha3.PXESpec{
				Image: host + "/edited-disk@" + liveDigest, NetbootImage: host + "/edited-netboot@" + liveDigest,
				Architecture: "amd64", PullSecretRef: secretRef("images", "live"), NetbootPullSecretRef: secretRef("images", "live"),
			}
			require.NoError(t, c.Update(ctx, machine))
			// Restart loses mappings, but not the files on disk.
			r = &OCIReconciler{Client: c, Cache: NewOCICache(cache.CacheDir), Site: "rack-a"}

			var machines v1alpha3.MachineList
			require.NoError(t, c.List(ctx, &machines))

			for i := range machines.Items {
				for _, req := range r.mapMachineToImage(ctx, &machines.Items[i]) {
					result, err := r.Reconcile(ctx, req)
					require.NoError(t, err)
					require.Equal(t, imageResyncInterval, result.RequeueAfter)
				}
			}

			_, err := r.Cache.MetadataForRefArchitecture(spec.NetbootImage, "arm64")
			require.ErrorIs(t, err, ErrNotYetDownloaded)

			if tt.missingSecret {
				require.NoError(t, c.Delete(ctx, secret))
			}

			var ops v1alpha3.MachineOperationList
			require.NoError(t, c.List(ctx, &ops))
			require.Len(t, ops.Items, 1)
			reqs := r.mapOperationToImage(ctx, &ops.Items[0])
			wantRequests := 0

			for _, wanted := range []bool{tt.wantDisk, tt.wantBoot} {
				if wanted {
					wantRequests++
				}
			}

			require.Len(t, reqs, wantRequests)

			for _, req := range reqs {
				result, err := r.Reconcile(ctx, req)
				require.NoError(t, err)

				if tt.missingSecret {
					require.Equal(t, 30*time.Second, result.RequeueAfter)
				} else {
					require.Equal(t, imageResyncInterval, result.RequeueAfter)
				}
			}

			if tt.missingSecret {
				require.Empty(t, r.Cache.DigestForArchitecture(spec.Image, "arm64"))
				require.Empty(t, r.Cache.DigestForArchitecture(spec.NetbootImage, "arm64"))

				secret.ResourceVersion = ""
				require.NoError(t, c.Create(ctx, secret))

				for _, req := range reqs {
					result, err := r.Reconcile(ctx, req)
					require.NoError(t, err)
					require.Equal(t, imageResyncInterval, result.RequeueAfter)
				}
			}

			for ref, payload := range map[string]string{spec.Image: "frozen disk", spec.NetbootImage: "frozen netboot"} {
				if (ref == spec.Image && !tt.wantDisk) || (ref == spec.NetbootImage && !tt.wantBoot) {
					require.Empty(t, r.Cache.DigestForArchitecture(ref, "arm64"))
					continue
				}

				_, err := r.Cache.MetadataForRefArchitecture(ref, "arm64")
				require.NoError(t, err)
				path, tmpl, err := r.Cache.ResolvePathForArchitecture(ref, "arm64", "disk.img")
				require.NoError(t, err)
				require.False(t, tmpl)

				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, payload, string(data))
				require.Empty(t, r.Cache.DigestForArchitecture(ref, "amd64"))
			}

			if tt.released {
				provider := &MediaProvider{Client: c, Site: "rack-a"}
				owner, claimed, err := provider.firstBootOwner(ctx, spec.DHCPLeases[0].IPv4)
				require.NoError(t, err)
				require.Equal(t, tt.wantBoot, claimed)

				if tt.wantBoot {
					require.Equal(t, spec, owner.machine.Spec.Netboot())

					resolver := &FileResolver{Cache: r.Cache, Reader: c, Cluster: &StaticClusterInfo{}}
					resolved, err := resolver.ResolveFileByPathForIP(ctx, "cloud-init/vendor-data", owner.machine, spec.NetbootImage, spec.DHCPLeases[0].IPv4)
					require.NoError(t, err)
					require.Equal(t, "frozen cloud-init node", string(resolved.Data))
				}
			}

			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(op), op))
			require.Equal(t, frozenSpec, op.Status.Targets[0].BootMedia.Netboot)
		})
	}
}
