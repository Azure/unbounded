// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

func TestSessionManagerSnapshotsDigestsAndReusesSession(t *testing.T) {
	t.Parallel()

	machine := testBareMetalMachine("machine-session", "rack-a")
	machine.UID = "machine-uid"
	machine.Generation = 4
	machine.Spec.PXE.EndpointRef = "rack-a-edge"
	machine.Spec.PXE.NetbootImage = "ghcr.io/test/netboot:v1"
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{
		Version:            "v1.35.0",
		NodeLabels:         map[string]string{"user-label": "original"},
		RegisterWithTaints: []string{"dedicated=metal:NoSchedule"},
	}
	machine.Spec.Agent = &v1alpha3.AgentSpec{Image: "ghcr.io/test/agent:v1", Version: "v1.2.3"}
	machine.Spec.PXE.CloudInit = &v1alpha3.CloudInitSpec{UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
		Name: "machine-session-user-data", Namespace: "default",
	}}
	op := testOperation("replace-session", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Generation = 2
	endpoint := readyTestEndpoint()

	cache := netboot.NewOCICache(t.TempDir())
	machineDigest := "sha256:" + stringOf('a', 64)
	netbootDigest := "sha256:" + stringOf('b', 64)
	cache.SetDigestForArchitecture(machine.Spec.PXE.Image, v1alpha3.DefaultPXEArchitecture, machineDigest)
	cache.SetDigestForArchitecture(machine.Spec.PXE.NetbootImage, v1alpha3.DefaultPXEArchitecture, netbootDigest)
	require.NoError(t, writeSessionMetadata(cache, netbootDigest, "http/bootx64.efi"))

	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(endpoint, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-session-user-data", Namespace: "default"},
		Data:       map[string]string{"user-data": "#cloud-config\nhostname: original\n"},
	}).WithStatusSubresource(&v1alpha3.NetbootSession{}).Build()
	manager := &KubernetesSessionManager{
		Client:            c,
		Cache:             cache,
		Cluster:           &netboot.StaticClusterInfo{Info: netboot.ClusterInfo{ApiserverURL: "https://api.example.com:6443", CACertBase64: "cluster-ca"}},
		KubernetesVersion: "v1.34.0",
		ClusterDNS:        "10.96.0.10",
		ProviderLabels:    map[string]string{"provider-label": "original"},
		Now:               func() metav1.Time { return fixedNow() },
	}

	session, err := manager.Ensure(t.Context(), op, machine)
	require.NoError(t, err)
	require.Equal(t, v1alpha3.NetbootSessionPhaseReady, session.Status.Phase)
	require.Equal(t, "sha256:"+stringOf('a', 64), session.Spec.Artifacts.MachineImage.Digest)
	require.Equal(t, "sha256:"+stringOf('b', 64), session.Spec.Artifacts.NetbootImage.Digest)
	require.Equal(t, machine.UID, session.Spec.Machine.UID)
	require.Equal(t, op.UID, session.Spec.Operation.UID)
	require.Equal(t, endpoint.Spec.ExternalURL, session.Spec.Endpoint.ExternalURL)
	require.Equal(t, "http/bootx64.efi", session.Spec.Boot.FirmwareArtifact)
	require.Equal(t, "https://api.example.com:6443", session.Spec.Provisioning.Cluster.APIServerURL)
	require.Equal(t, "cluster-ca", session.Spec.Provisioning.Cluster.CACertBase64)
	require.Equal(t, "10.96.0.10", session.Spec.Provisioning.Cluster.DNS)
	require.Equal(t, "v1.34.0", session.Spec.Provisioning.Cluster.KubernetesVersion)
	require.Equal(t, "original", session.Spec.Provisioning.Kubernetes.NodeLabels["user-label"])
	require.Equal(t, "ghcr.io/test/agent:v1", session.Spec.Provisioning.Agent.Image)
	require.Equal(t, "original", session.Spec.Provisioning.ProviderLabels["provider-label"])
	require.Equal(t, "#cloud-config\nhostname: original\n", session.Spec.Provisioning.UserData)
	require.Contains(t, session.Spec.Artifacts.Files, v1alpha3.NetbootSessionArtifact{
		Name:   "http/bootx64.efi",
		Source: "NetbootImage",
		Path:   "/disk/http/bootx64.efi",
	})
	for _, name := range []string{
		"vmlinuz", "initrd", "init.cpio", "grub/grub.cfg", "grubx64.efi",
		"cloud-init/meta-data", "cloud-init/user-data", "cloud-init/vendor-data", "cloud-init/network-config",
	} {
		require.Condition(t, func() bool {
			for _, artifact := range session.Spec.Artifacts.Files {
				if artifact.Name == name {
					return true
				}
			}

			return false
		}, "session artifact list is missing %s", name)
	}
	require.True(t, session.Spec.ExpiresAt.Time.Equal(fixedNow().Add(24*time.Hour)))

	machine.Spec.PXE.Image = "ghcr.io/test/changed:v2"
	machine.Spec.Kubernetes.NodeLabels["user-label"] = "changed"
	machine.Spec.Agent.Image = "ghcr.io/test/agent:changed"
	manager.ProviderLabels["provider-label"] = "changed"
	reused, err := manager.Ensure(t.Context(), op, machine)
	require.NoError(t, err)
	require.Equal(t, session.Name, reused.Name)
	require.Equal(t, "ghcr.io/test/host:v1", reused.Spec.Artifacts.MachineImage.Reference)
	require.Equal(t, "original", reused.Spec.Provisioning.Kubernetes.NodeLabels["user-label"])
	require.Equal(t, "ghcr.io/test/agent:v1", reused.Spec.Provisioning.Agent.Image)
	require.Equal(t, "original", reused.Spec.Provisioning.ProviderLabels["provider-label"])

	var sessions v1alpha3.NetbootSessionList
	require.NoError(t, c.List(t.Context(), &sessions, client.MatchingLabels{sessionOperationUIDLabel: string(op.UID)}))
	require.Len(t, sessions.Items, 1)
}

func TestSessionManagerWaitsForImmutableDigests(t *testing.T) {
	t.Parallel()

	machine := testBareMetalMachine("machine-pending", "rack-a")
	machine.UID = "machine-uid"
	machine.Generation = 1
	machine.Spec.PXE.EndpointRef = "rack-a-edge"
	machine.Spec.PXE.NetbootImage = "ghcr.io/test/netboot:v1"
	op := testOperation("replace-pending", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Generation = 1

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(readyTestEndpoint()).WithStatusSubresource(&v1alpha3.NetbootSession{}).Build()
	manager := &KubernetesSessionManager{Client: c, Cache: netboot.NewOCICache(t.TempDir())}

	_, err := manager.Ensure(t.Context(), op, machine)
	require.ErrorIs(t, err, netboot.ErrNotYetDownloaded)

	var sessions v1alpha3.NetbootSessionList
	require.NoError(t, c.List(t.Context(), &sessions))
	require.Empty(t, sessions.Items)
}

func TestSessionManagerPromotesExistingSessionWhenEndpointBecomesReady(t *testing.T) {
	t.Parallel()

	machine := testBareMetalMachine("machine-endpoint", "rack-a")
	machine.UID = "machine-uid"
	machine.Generation = 1
	machine.Spec.PXE.EndpointRef = "rack-a-edge"
	machine.Spec.PXE.NetbootImage = "ghcr.io/test/netboot:v1"
	op := testOperation("replace-endpoint", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Generation = 1
	endpoint := readyTestEndpoint()
	endpoint.Status.Conditions[0].Status = metav1.ConditionFalse

	cache := netboot.NewOCICache(t.TempDir())
	cache.SetDigestForArchitecture(machine.Spec.PXE.Image, v1alpha3.DefaultPXEArchitecture, "sha256:"+stringOf('a', 64))
	cache.SetDigestForArchitecture(machine.Spec.PXE.NetbootImage, v1alpha3.DefaultPXEArchitecture, "sha256:"+stringOf('b', 64))
	require.NoError(t, writeSessionMetadata(cache, "sha256:"+stringOf('b', 64), "bootx64.efi"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(endpoint).WithStatusSubresource(endpoint, &v1alpha3.NetbootSession{}).Build()
	manager := &KubernetesSessionManager{Client: c, Cache: cache}

	session, err := manager.Ensure(t.Context(), op, machine)
	require.NoError(t, err)
	require.Equal(t, v1alpha3.NetbootSessionPhasePreparing, session.Status.Phase)

	endpoint.Status.Conditions[0].Status = metav1.ConditionTrue
	require.NoError(t, c.Status().Update(t.Context(), endpoint))

	session, err = manager.Ensure(t.Context(), op, machine)
	require.NoError(t, err)
	require.Equal(t, v1alpha3.NetbootSessionPhaseReady, session.Status.Phase)
}

func writeSessionMetadata(cache *netboot.OCICache, digest, httpBootPath string) error {
	diskDir := cache.DiskDirForArchitecture(digest, v1alpha3.DefaultPXEArchitecture)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(diskDir, "metadata.yaml"), []byte("dhcpBootImageName: "+httpBootPath+"\nhttpBootPath: "+httpBootPath+"\n"), 0o600)
}

func readyTestEndpoint() *v1alpha3.NetbootEndpoint {
	return &v1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a-edge", UID: "endpoint-uid", Generation: 3},
		Spec: v1alpha3.NetbootEndpointSpec{
			SiteRef:     "rack-a",
			Type:        v1alpha3.NetbootEndpointTypeExternalL2,
			ExternalURL: "http://192.0.2.10:8880",
			TLS: v1alpha3.NetbootEndpointTLS{
				Trust: v1alpha3.NetbootEndpointTrustTrustedLAN,
				Mode:  v1alpha3.NetbootEndpointTLSDisabled,
			},
		},
		Status: v1alpha3.NetbootEndpointStatus{
			ObservedGeneration: 3,
			Conditions: []metav1.Condition{{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "Available",
			}},
		},
	}
}

func stringOf(value byte, count int) string {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}

	return string(result)
}
