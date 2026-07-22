// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

const (
	sessionOperationUIDLabel = "metalman.unbounded-cloud.io/operation-uid"
	sessionMachineUIDLabel   = "metalman.unbounded-cloud.io/machine-uid"
	defaultSessionTTL        = 24 * time.Hour
)

// KubernetesSessionManager persists immutable sessions in the Kubernetes API.
type KubernetesSessionManager struct {
	Client                   client.Client
	Cache                    *netboot.OCICache
	DefaultNetbootRef        string
	DefaultNetbootPullSecret *v1alpha3.NamespacedSecretReference
	Cluster                  netboot.ClusterInfoProvider
	KubernetesVersion        string
	ClusterDNS               string
	ProviderLabels           map[string]string
	Now                      func() metav1.Time
}

// Ensure returns the durable session for one operation target, creating it
// only after both OCI references have resolved to immutable digests.
func (m *KubernetesSessionManager) Ensure(ctx context.Context, operation *v1alpha3.MachineOperation, machine *v1alpha3.Machine) (*v1alpha3.NetbootSession, error) {
	if operation.UID == "" || machine.UID == "" {
		return nil, errors.New("operation and Machine UIDs are required for netboot session identity")
	}

	name := sessionName(operation.UID, machine.UID)

	var existing v1alpha3.NetbootSession
	if err := m.Client.Get(ctx, client.ObjectKey{Name: name}, &existing); err == nil {
		if existing.Spec.Operation.UID != operation.UID || existing.Spec.Machine.UID != machine.UID {
			return nil, fmt.Errorf("netboot session %s belongs to different objects", name)
		}

		if existing.Status.Phase == v1alpha3.NetbootSessionPhasePreparing {
			return m.refreshEndpointReadiness(ctx, &existing)
		}

		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get netboot session %s: %w", name, err)
	}

	netbootSpec := machine.Spec.Netboot()
	if netbootSpec == nil {
		return nil, errors.New("Machine has no netboot configuration")
	}

	var endpoint v1alpha3.NetbootEndpoint
	if err := m.Client.Get(ctx, client.ObjectKey{Name: netbootSpec.EndpointRef}, &endpoint); err != nil {
		return nil, fmt.Errorf("get NetbootEndpoint %s: %w", netbootSpec.EndpointRef, err)
	}

	architecture := netbootSpec.TargetArchitecture()
	machineDigest := m.Cache.DigestForArchitecture(netbootSpec.Image, architecture)
	if machineDigest == "" {
		return nil, fmt.Errorf("%w: machine image %q for architecture %q", netboot.ErrNotYetDownloaded, netbootSpec.Image, architecture)
	}

	netbootRef := netbootSpec.NetbootImage
	netbootPullSecret := netbootSpec.NetbootPullSecretRef
	if netbootRef == "" {
		netbootRef = m.DefaultNetbootRef
		netbootPullSecret = m.DefaultNetbootPullSecret
	}

	netbootDigest := m.Cache.DigestForArchitecture(netbootRef, architecture)
	if netbootDigest == "" {
		return nil, fmt.Errorf("%w: netboot image %q for architecture %q", netboot.ErrNotYetDownloaded, netbootRef, architecture)
	}
	metadata, err := m.Cache.MetadataForArchitecture(netbootDigest, architecture)
	if err != nil {
		return nil, fmt.Errorf("read netboot image metadata: %w", err)
	}
	firmwareArtifact := firmwareArtifactForTransport(metadata, netbootSpec.TargetTransport())
	if firmwareArtifact == "" {
		return nil, fmt.Errorf("netboot image %q has no firmware artifact for %s", netbootRef, netbootSpec.TargetTransport())
	}

	now := m.now()
	userData, err := m.resolveUserData(ctx, netbootSpec)
	if err != nil {
		return nil, err
	}
	clusterInfo := netboot.ClusterInfo{}
	if m.Cluster != nil {
		clusterInfo = m.Cluster.ClusterInfo()
	}
	session := &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				sessionOperationUIDLabel: string(operation.UID),
				sessionMachineUIDLabel:   string(machine.UID),
			},
		},
		Spec: v1alpha3.NetbootSessionSpec{
			Machine:   objectSnapshot(machine.Name, machine.UID, machine.Generation),
			Operation: objectSnapshot(operation.Name, operation.UID, operation.Generation),
			Endpoint: v1alpha3.NetbootSessionEndpointSnapshot{
				Name:        endpoint.Name,
				UID:         endpoint.UID,
				ExternalURL: endpoint.Spec.ExternalURL,
			},
			Boot: v1alpha3.NetbootSessionBoot{
				Transport:           netbootSpec.TargetTransport(),
				ConfigurationSource: targetConfigurationSource(netbootSpec),
				NetworkMode:         targetNetworkMode(netbootSpec),
				FirmwareArtifact:    firmwareArtifact,
				Architecture:        architecture,
				DHCPLeases:          append([]v1alpha3.DHCPLease(nil), netbootSpec.DHCPLeases...),
				TargetDisk:          netbootSpec.TargetDisk,
			},
			Provisioning: v1alpha3.NetbootSessionProvisioning{
				Cluster: v1alpha3.NetbootSessionCluster{
					APIServerURL:      clusterInfo.ApiserverURL,
					CACertBase64:      clusterInfo.CACertBase64,
					DNS:               m.ClusterDNS,
					KubernetesVersion: m.KubernetesVersion,
				},
				Kubernetes:     machine.Spec.Kubernetes.DeepCopy(),
				Agent:          machine.Spec.Agent.DeepCopy(),
				ProviderLabels: maps.Clone(m.ProviderLabels),
				UserData:       userData,
			},
			Artifacts: v1alpha3.NetbootSessionArtifacts{
				MachineImage: v1alpha3.NetbootSessionImage{
					Reference:     netbootSpec.Image,
					Digest:        machineDigest,
					PullSecretRef: netbootSpec.PullSecretRef.DeepCopy(),
				},
				NetbootImage: v1alpha3.NetbootSessionImage{
					Reference:     netbootRef,
					Digest:        netbootDigest,
					PullSecretRef: netbootPullSecret.DeepCopy(),
				},
				Files: sessionArtifacts(firmwareArtifact),
			},
			ExpiresAt: metav1.NewTime(now.Add(defaultSessionTTL)),
		},
	}

	if err := m.Client.Create(ctx, session); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := m.Client.Get(ctx, client.ObjectKey{Name: name}, session); err != nil {
				return nil, fmt.Errorf("get concurrently created netboot session %s: %w", name, err)
			}

			return session, nil
		}

		return nil, fmt.Errorf("create netboot session %s: %w", name, err)
	}

	return m.refreshEndpointReadinessWithEndpoint(ctx, session, &endpoint)
}

func (m *KubernetesSessionManager) resolveUserData(ctx context.Context, spec *v1alpha3.PXESpec) (string, error) {
	if spec.CloudInit == nil || spec.CloudInit.UserDataConfigMapRef == nil {
		return "#cloud-config\n", nil
	}

	ref := spec.CloudInit.UserDataConfigMapRef
	var configMap corev1.ConfigMap
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &configMap); err != nil {
		return "", fmt.Errorf("get cloud-init user-data ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	key := ref.Key
	if key == "" {
		key = "user-data"
	}
	if value, ok := configMap.Data[key]; ok {
		return value, nil
	}
	if value, ok := configMap.BinaryData[key]; ok {
		return string(value), nil
	}

	return "", fmt.Errorf("cloud-init user-data key %q not found in ConfigMap %s/%s", key, ref.Namespace, ref.Name)
}

func (m *KubernetesSessionManager) refreshEndpointReadiness(ctx context.Context, session *v1alpha3.NetbootSession) (*v1alpha3.NetbootSession, error) {
	var endpoint v1alpha3.NetbootEndpoint
	if err := m.Client.Get(ctx, client.ObjectKey{Name: session.Spec.Endpoint.Name}, &endpoint); err != nil {
		return nil, fmt.Errorf("get NetbootEndpoint %s: %w", session.Spec.Endpoint.Name, err)
	}

	if endpoint.UID != session.Spec.Endpoint.UID {
		return nil, fmt.Errorf("NetbootEndpoint %s identity changed", endpoint.Name)
	}

	return m.refreshEndpointReadinessWithEndpoint(ctx, session, &endpoint)
}

func (m *KubernetesSessionManager) refreshEndpointReadinessWithEndpoint(ctx context.Context, session *v1alpha3.NetbootSession, endpoint *v1alpha3.NetbootEndpoint) (*v1alpha3.NetbootSession, error) {
	now := m.now()

	ready := endpoint.Status.ObservedGeneration == endpoint.Generation && apimeta.IsStatusConditionTrue(endpoint.Status.Conditions, "Ready")
	session.Status.Phase = v1alpha3.NetbootSessionPhasePreparing
	if ready {
		session.Status.Phase = v1alpha3.NetbootSessionPhaseReady
	}

	apimeta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.NetbootSessionConditionPrepared,
		Status:             metav1.ConditionTrue,
		Reason:             "ArtifactsResolved",
		Message:            "OCI references resolved to immutable digests",
		ObservedGeneration: session.Generation,
		LastTransitionTime: now,
	})
	apimeta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.NetbootSessionConditionEndpointReady,
		Status:             conditionStatus(ready),
		Reason:             endpointReadyReason(ready),
		Message:            fmt.Sprintf("NetbootEndpoint %s readiness", endpoint.Name),
		ObservedGeneration: session.Generation,
		LastTransitionTime: now,
	})

	if err := m.Client.Status().Update(ctx, session); err != nil {
		return nil, fmt.Errorf("update netboot session %s status: %w", session.Name, err)
	}

	return session, nil
}

func (m *KubernetesSessionManager) now() metav1.Time {
	if m.Now != nil {
		return m.Now()
	}

	return metav1.Now()
}

func sessionName(operationUID, machineUID types.UID) string {
	return fmt.Sprintf("netboot-%s-%s", operationUID, machineUID)
}

func objectSnapshot(name string, uid types.UID, generation int64) v1alpha3.NetbootSessionObjectSnapshot {
	return v1alpha3.NetbootSessionObjectSnapshot{Name: name, UID: uid, Generation: generation}
}

func targetConfigurationSource(spec *v1alpha3.PXESpec) v1alpha3.NetbootConfigurationSource {
	if spec.ConfigurationSource == "" {
		return v1alpha3.NetbootConfigurationSourceDHCP
	}

	return spec.ConfigurationSource
}

func targetNetworkMode(spec *v1alpha3.PXESpec) v1alpha3.NetbootNetworkMode {
	if spec.NetworkMode == "" {
		return v1alpha3.NetbootNetworkModeDHCP
	}

	return spec.NetworkMode
}

func sessionArtifacts(firmwareArtifact string) []v1alpha3.NetbootSessionArtifact {
	return []v1alpha3.NetbootSessionArtifact{
		{Name: "disk.img.gz", Source: "MachineImage", Path: "/disk/disk.img.gz"},
		{Name: "metadata.yaml", Source: "NetbootImage", Path: "/disk/metadata.yaml"},
		{Name: firmwareArtifact, Source: "NetbootImage", Path: "/disk/" + firmwareArtifact},
	}
}

func firmwareArtifactForTransport(metadata *netboot.ImageMetadata, transport v1alpha3.NetbootTransport) string {
	if metadata == nil {
		return ""
	}
	if transport == v1alpha3.NetbootTransportHTTP {
		return netboot.HTTPBootPathFromMetadata(metadata)
	}

	return strings.TrimPrefix(metadata.DHCPBootImageName, "/")
}

func conditionStatus(ready bool) metav1.ConditionStatus {
	if ready {
		return metav1.ConditionTrue
	}

	return metav1.ConditionFalse
}

func endpointReadyReason(ready bool) string {
	if ready {
		return "Ready"
	}

	return "NotReady"
}
