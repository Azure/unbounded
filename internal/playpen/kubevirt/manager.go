// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/labels"
)

const (
	DefaultTTL = time.Hour

	BootTargetPxe      = "Pxe"
	BootTargetHdd      = "Hdd"
	BootTargetUefiHTTP = "UefiHttp"

	BootContinuous = "Continuous"
	BootOnce       = "Once"
	BootDisabled   = "Disabled"
	BootModeUEFI   = "UEFI"

	PowerOn  = "On"
	PowerOff = "Off"

	DefaultL2VXLANPort       = 4790
	DefaultL2VXLANVNI        = 100
	DefaultL2BridgeInterface = "br-playpen"
	DefaultL2AttachInterface = "net1"
	DefaultL2VXLANInterface  = "vxlan-l2"
)

type Config struct {
	Namespace             string
	ServiceName           string
	ServicePort           int
	DefaultVMImage        string
	DefaultNetwork        string
	DefaultPodCIDRBase    string
	DefaultSite           string
	DefaultGatewayPool    string
	DefaultSSHKey         string
	HTTPBootContainerDisk string
	L2EndpointImage       string
	L2VXLANPort           int
	L2VXLANVNI            int
	L2BridgeInterface     string
	L2AttachInterface     string
	L2VXLANInterface      string
}

type Manager struct {
	ctrl client.Client
	cfg  Config
}

type AllocateRequest struct {
	NamePrefix            string
	Site                  string
	PodCIDR               string
	VMImage               string
	NetworkAttachmentName string
	SSHAuthorizedKey      string
	TTLSeconds            int64
	L2Tunnel              L2TunnelConfig
}

type Allocation struct {
	ID        string
	VMName    string
	Secret    string
	Namespace string
	Site      string
	PodCIDR   string
	MAC       string
	Username  string
	Password  string
	ExpiresAt time.Time
	L2Tunnel  L2TunnelConfig
}

type L2TunnelConfig struct {
	Enabled               bool   `json:"enabled"`
	Mode                  string `json:"mode,omitempty"`
	NetworkAttachmentName string `json:"networkAttachmentName,omitempty"`
	EndpointNamespace     string `json:"endpointNamespace,omitempty"`
	EndpointPodName       string `json:"endpointPodName,omitempty"`
	EndpointUnderlayIP    string `json:"endpointUnderlayIP,omitempty"`
	ClientUnderlayIP      string `json:"clientUnderlayIP,omitempty"`
	VXLANVNI              int    `json:"vxlanVNI,omitempty"`
	VXLANPort             int    `json:"vxlanPort,omitempty"`
	BridgeInterface       string `json:"bridgeInterface,omitempty"`
	AttachInterface       string `json:"attachInterface,omitempty"`
	VXLANInterface        string `json:"vxlanInterface,omitempty"`
}

type BootConfig struct {
	Target      string
	Enabled     string
	Mode        string
	HTTPBootURI string
}

func NewManager(ctrlClient client.Client, cfg Config) *Manager {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "playpen"
	}
	if cfg.ServicePort == 0 {
		cfg.ServicePort = 9443
	}
	if cfg.DefaultVMImage == "" {
		cfg.DefaultVMImage = "quay.io/containerdisks/fedora:latest"
	}
	if cfg.DefaultNetwork == "" {
		cfg.DefaultNetwork = "default/playpen-net"
	}
	if cfg.DefaultPodCIDRBase == "" {
		cfg.DefaultPodCIDRBase = "10.241.0.0/16"
	}
	if cfg.DefaultSite == "" {
		cfg.DefaultSite = "playpen"
	}

	return &Manager{ctrl: ctrlClient, cfg: cfg}
}

func (m *Manager) Allocate(ctx context.Context, req AllocateRequest) (*Allocation, error) {
	allocID := newAllocationID(req.NamePrefix)
	password := uuid.NewString()

	ttl := DefaultTTL
	if req.TTLSeconds > 0 && req.TTLSeconds < int64(DefaultTTL.Seconds()) {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}

	vmName := allocID
	secretName := allocID + "-redfish"
	site := firstNonEmpty(req.Site, m.cfg.DefaultSite)
	podCIDR := firstNonEmpty(req.PodCIDR, podCIDRForAllocation(m.cfg.DefaultPodCIDRBase, allocID))
	mac := macForAllocation(allocID)
	expiresAt := time.Now().UTC().Add(ttl)
	vmImage := firstNonEmpty(req.VMImage, m.cfg.DefaultVMImage)
	networkName := firstNonEmpty(req.NetworkAttachmentName, m.cfg.DefaultNetwork)
	sshKey := firstNonEmpty(req.SSHAuthorizedKey, m.cfg.DefaultSSHKey)
	l2Tunnel := m.l2TunnelConfig(allocID, networkName, podCIDR, req.L2Tunnel)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: m.cfg.Namespace,
			Labels:    ownedLabels(allocID, "redfish-secret"),
			Annotations: map[string]string{
				labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339),
			},
		},
		StringData: map[string]string{
			"username": "playpen",
			"password": password,
		},
	}
	if err := m.ctrl.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create redfish secret: %w", err)
	}
	if l2Tunnel.Enabled {
		endpointIP, err := m.createL2Endpoint(ctx, allocID, expiresAt, l2Tunnel)
		if err != nil {
			_ = m.ctrl.Delete(ctx, secret)
			return nil, err
		}
		l2Tunnel.EndpointUnderlayIP = endpointIP
	}

	vm := buildVM(vmBuildInput{
		Name:                  vmName,
		Namespace:             m.cfg.Namespace,
		AllocationID:          allocID,
		Image:                 vmImage,
		NetworkAttachmentName: networkName,
		MAC:                   mac,
		SSHAuthorizedKey:      sshKey,
		ExpiresAt:             expiresAt,
		Site:                  site,
		PodCIDR:               podCIDR,
		HTTPBootContainerDisk: m.cfg.HTTPBootContainerDisk,
		L2Tunnel:              l2Tunnel,
	})
	if err := m.ctrl.Create(ctx, vm); err != nil {
		if l2Tunnel.Enabled {
			_ = m.ctrl.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: l2Tunnel.EndpointPodName, Namespace: l2Tunnel.EndpointNamespace}})
		}
		_ = m.ctrl.Delete(ctx, secret)
		return nil, fmt.Errorf("create kubevirt vm: %w", err)
	}

	return &Allocation{
		ID:        allocID,
		VMName:    vmName,
		Secret:    secretName,
		Namespace: m.cfg.Namespace,
		Site:      site,
		PodCIDR:   podCIDR,
		MAC:       mac,
		Username:  "playpen",
		Password:  password,
		ExpiresAt: expiresAt,
		L2Tunnel:  l2Tunnel,
	}, nil
}

func (m *Manager) Delete(ctx context.Context, allocationID string) (bool, error) {
	if allocationID == "" {
		return false, fmt.Errorf("allocationID is required")
	}

	deleted := false
	vm := &kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: allocationID, Namespace: m.cfg.Namespace}}
	if err := m.ctrl.Delete(ctx, vm); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete vm: %w", err)
		}
	} else {
		deleted = true
	}

	secretName := allocationID + "-redfish"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: m.cfg.Namespace}}
	if err := m.ctrl.Delete(ctx, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("delete redfish secret: %w", err)
		}
	} else {
		deleted = true
	}

	endpoint := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: allocationID + "-l2", Namespace: m.cfg.Namespace}}
	if err := m.ctrl.Delete(ctx, endpoint); err != nil {
		if !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("delete l2 endpoint pod: %w", err)
		}
	} else {
		deleted = true
	}

	return deleted, nil
}

func (m *Manager) DeleteExpired(ctx context.Context, now time.Time) error {
	list := &kubevirtv1.VirtualMachineList{}
	if err := m.ctrl.List(ctx, list, client.InNamespace(m.cfg.Namespace), client.MatchingLabels{labels.OwnedLabel: "true"}); err != nil {
		return fmt.Errorf("list playpen vms: %w", err)
	}

	for i := range list.Items {
		vm := &list.Items[i]
		expiresRaw := vm.Annotations[labels.ExpiresAtAnnotation]
		expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
		if err != nil || now.Before(expiresAt) {
			continue
		}

		if _, err := m.Delete(ctx, vm.Labels[labels.AllocationIDLabel]); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) Systems(ctx context.Context) ([]string, error) {
	list := &kubevirtv1.VirtualMachineList{}
	if err := m.ctrl.List(ctx, list, client.InNamespace(m.cfg.Namespace), client.MatchingLabels{labels.OwnedLabel: "true"}); err != nil {
		return nil, fmt.Errorf("list playpen systems: %w", err)
	}

	systems := make([]string, 0, len(list.Items))
	for i := range list.Items {
		name := list.Items[i].Name
		if name != "" {
			systems = append(systems, name)
		}
	}

	return systems, nil
}

func (m *Manager) PowerState(ctx context.Context, allocationID string) (string, error) {
	vmi := &kubevirtv1.VirtualMachineInstance{}
	err := m.ctrl.Get(ctx, client.ObjectKey{Name: allocationID, Namespace: m.cfg.Namespace}, vmi)
	if apierrors.IsNotFound(err) {
		return PowerOff, nil
	}
	if err != nil {
		return "", fmt.Errorf("get vmi: %w", err)
	}

	if vmi.Status.Phase == kubevirtv1.Failed || vmi.Status.Phase == kubevirtv1.Succeeded {
		return PowerOff, nil
	}
	if err := m.consumeBootOnce(ctx, allocationID); err != nil {
		return "", err
	}

	return PowerOn, nil
}

func (m *Manager) SetPower(ctx context.Context, allocationID string, on bool) error {
	vm := &kubevirtv1.VirtualMachine{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: allocationID, Namespace: m.cfg.Namespace}, vm); err != nil {
		return fmt.Errorf("get vm: %w", err)
	}
	before := vm.DeepCopy()
	vm.Spec.Running = ptr.To(on)
	if err := m.ctrl.Patch(ctx, vm, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("patch vm power: %w", err)
	}

	return nil
}

func (m *Manager) BootConfig(ctx context.Context, allocationID string) (BootConfig, error) {
	vm := &kubevirtv1.VirtualMachine{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: allocationID, Namespace: m.cfg.Namespace}, vm); err != nil {
		return BootConfig{}, fmt.Errorf("get vm: %w", err)
	}

	ann := vm.Annotations
	return BootConfig{
		Target:      firstNonEmpty(ann[labels.BootTargetAnnotation], BootTargetHdd),
		Enabled:     firstNonEmpty(ann[labels.BootEnabledAnnotation], BootDisabled),
		Mode:        ann[labels.BootModeAnnotation],
		HTTPBootURI: ann[labels.HTTPBootURIAnnotation],
	}, nil
}

func (m *Manager) SetBootConfig(ctx context.Context, allocationID string, cfg BootConfig) error {
	cfg = normalizeBootConfig(cfg)
	if cfg.Target == "" {
		current, err := m.BootConfig(ctx, allocationID)
		if err != nil {
			return err
		}

		cfg.Target = current.Target
	}
	if cfg.Enabled == "" {
		cfg.Enabled = BootContinuous
	}
	cfg = normalizeBootConfig(cfg)

	annotations := map[string]any{
		labels.BootTargetAnnotation:  cfg.Target,
		labels.BootEnabledAnnotation: cfg.Enabled,
		labels.BootModeAnnotation:    nil,
		labels.HTTPBootURIAnnotation: nil,
	}
	if cfg.Mode != "" {
		annotations[labels.BootModeAnnotation] = cfg.Mode
	}
	if cfg.HTTPBootURI != "" {
		annotations[labels.HTTPBootURIAnnotation] = cfg.HTTPBootURI
	}

	vm := &kubevirtv1.VirtualMachine{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: allocationID, Namespace: m.cfg.Namespace}, vm); err != nil {
		return fmt.Errorf("get vm: %w", err)
	}
	before := vm.DeepCopy()
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		if value == nil {
			delete(vm.Annotations, key)
			continue
		}
		vm.Annotations[key] = value.(string)
	}
	applyBootSpec(vm, cfg, m.cfg.HTTPBootContainerDisk)
	if err := m.ctrl.Patch(ctx, vm, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("patch vm boot config: %w", err)
	}

	return nil
}

func normalizeBootConfig(cfg BootConfig) BootConfig {
	if cfg.Enabled == BootDisabled {
		cfg.Target = BootTargetHdd
		cfg.Mode = ""
		cfg.HTTPBootURI = ""
		return cfg
	}
	if cfg.Target == BootTargetUefiHTTP && cfg.Mode == "" {
		cfg.Mode = BootModeUEFI
	}
	if cfg.Target != BootTargetUefiHTTP {
		cfg.HTTPBootURI = ""
	}

	return cfg
}

func (m *Manager) RedfishCredentials(ctx context.Context, allocationID string) (string, string, error) {
	secret := &corev1.Secret{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: allocationID + "-redfish", Namespace: m.cfg.Namespace}, secret); err != nil {
		return "", "", fmt.Errorf("get redfish secret: %w", err)
	}

	return string(secret.Data["username"]), string(secret.Data["password"]), nil
}

func (m *Manager) AllocationIDForRedfishCredentials(ctx context.Context, username, password string) (string, error) {
	secrets := &corev1.SecretList{}
	if err := m.ctrl.List(ctx, secrets, client.InNamespace(m.cfg.Namespace), client.MatchingLabels{labels.OwnedLabel: "true"}); err != nil {
		return "", fmt.Errorf("list redfish secrets: %w", err)
	}

	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Labels[labels.ComponentLabel] != "redfish-secret" {
			continue
		}
		if string(secret.Data["username"]) == username && string(secret.Data["password"]) == password {
			allocationID := secret.Labels[labels.AllocationIDLabel]
			if allocationID == "" {
				return "", fmt.Errorf("redfish secret %s is missing allocation label", secret.Name)
			}

			return allocationID, nil
		}
	}

	return "", apierrors.NewUnauthorized("invalid redfish credentials")
}

func (m *Manager) RedfishURL(_ string) string {
	return fmt.Sprintf("https://%s.%s.svc:%d", m.cfg.ServiceName, m.cfg.Namespace, m.cfg.ServicePort)
}

type vmBuildInput struct {
	Name                  string
	Namespace             string
	AllocationID          string
	Image                 string
	NetworkAttachmentName string
	MAC                   string
	SSHAuthorizedKey      string
	ExpiresAt             time.Time
	Site                  string
	PodCIDR               string
	HTTPBootContainerDisk string
	L2Tunnel              L2TunnelConfig
}

func buildVM(in vmBuildInput) *kubevirtv1.VirtualMachine {
	labelsMap := ownedLabels(in.AllocationID, "vm")
	annotations := map[string]string{
		labels.ExpiresAtAnnotation:       in.ExpiresAt.Format(time.RFC3339),
		labels.MACAddressAnnotation:      in.MAC,
		labels.RedfishUsernameAnnotation: "playpen",
		labels.BootTargetAnnotation:      BootTargetHdd,
		labels.BootEnabledAnnotation:     BootDisabled,
		labels.SiteAnnotation:            in.Site,
		labels.PodCIDRAnnotation:         in.PodCIDR,
	}
	if in.L2Tunnel.Enabled {
		if data, err := json.Marshal(in.L2Tunnel); err == nil {
			annotations[labels.L2TunnelAnnotation] = string(data)
		}
	}

	disks := []kubevirtv1.Disk{
		{Name: "rootdisk", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio}}, BootOrder: ptr.To(uint(1))},
		{Name: "cloudinitdisk", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio}}},
	}
	volumes := []kubevirtv1.Volume{
		{Name: "rootdisk", VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: in.Image}}},
		{Name: "cloudinitdisk", VolumeSource: kubevirtv1.VolumeSource{CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{UserData: cloudInit(in.SSHAuthorizedKey)}}},
	}
	if in.HTTPBootContainerDisk != "" {
		disks = append(disks, kubevirtv1.Disk{Name: "httpbootdisk", DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA}}, BootOrder: ptr.To(uint(3))})
		volumes = append(volumes, kubevirtv1.Volume{Name: "httpbootdisk", VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: in.HTTPBootContainerDisk}}})
	}

	return &kubevirtv1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: kubevirtv1.GroupVersion.String(), Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        in.Name,
			Namespace:   in.Namespace,
			Labels:      labelsMap,
			Annotations: annotations,
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			Running: ptr.To(false),
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsMap},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						CPU:       &kubevirtv1.CPU{Cores: 1},
						Resources: kubevirtv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
						Firmware:  &kubevirtv1.Firmware{Bootloader: &kubevirtv1.Bootloader{EFI: &kubevirtv1.EFI{SecureBoot: ptr.To(false)}}},
						Devices: kubevirtv1.Devices{
							Disks: disks,
							Interfaces: []kubevirtv1.Interface{{
								Name:                   "playpen",
								MacAddress:             in.MAC,
								InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Bridge: &kubevirtv1.InterfaceBridge{}},
								BootOrder:              ptr.To(uint(2)),
							}},
						},
					},
					Networks: []kubevirtv1.Network{{
						Name:          "playpen",
						NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: in.NetworkAttachmentName}},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func (m *Manager) l2TunnelConfig(allocationID, networkName, podCIDR string, in L2TunnelConfig) L2TunnelConfig {
	if !in.Enabled && m.cfg.L2EndpointImage == "" {
		return L2TunnelConfig{}
	}
	in.Enabled = true
	in.Mode = firstNonEmpty(in.Mode, "endpoint-pod")
	in.NetworkAttachmentName = firstNonEmpty(in.NetworkAttachmentName, networkName)
	in.EndpointNamespace = firstNonEmpty(in.EndpointNamespace, m.cfg.Namespace)
	in.EndpointPodName = firstNonEmpty(in.EndpointPodName, allocationID+"-l2")
	in.ClientUnderlayIP = firstNonEmpty(in.ClientUnderlayIP, routerIP(podCIDR))
	if in.VXLANVNI == 0 {
		in.VXLANVNI = firstNonZero(m.cfg.L2VXLANVNI, DefaultL2VXLANVNI)
	}
	if in.VXLANPort == 0 {
		in.VXLANPort = firstNonZero(m.cfg.L2VXLANPort, DefaultL2VXLANPort)
	}
	in.BridgeInterface = firstNonEmpty(in.BridgeInterface, m.cfg.L2BridgeInterface, DefaultL2BridgeInterface)
	in.AttachInterface = firstNonEmpty(in.AttachInterface, m.cfg.L2AttachInterface, DefaultL2AttachInterface)
	in.VXLANInterface = firstNonEmpty(in.VXLANInterface, m.cfg.L2VXLANInterface, DefaultL2VXLANInterface)

	return in
}

func (m *Manager) createL2Endpoint(ctx context.Context, allocationID string, expiresAt time.Time, l2 L2TunnelConfig) (string, error) {
	if m.cfg.L2EndpointImage == "" {
		return "", fmt.Errorf("l2 endpoint image is required")
	}
	data, err := json.Marshal(l2)
	if err != nil {
		return "", fmt.Errorf("marshal l2 tunnel config: %w", err)
	}

	pod := buildL2EndpointPod(l2EndpointPodInput{
		AllocationID: allocationID,
		Namespace:    l2.EndpointNamespace,
		Name:         l2.EndpointPodName,
		Image:        m.cfg.L2EndpointImage,
		Network:      l2.NetworkAttachmentName,
		L2TunnelJSON: string(data),
		ExpiresAt:    expiresAt,
	})
	if err := m.ctrl.Create(ctx, pod); err != nil {
		return "", fmt.Errorf("create l2 endpoint pod: %w", err)
	}

	endpointIP, err := m.waitForL2EndpointIP(ctx, l2.EndpointNamespace, l2.EndpointPodName)
	if err != nil {
		_ = m.ctrl.Delete(context.Background(), pod)
		return "", err
	}

	return endpointIP, nil
}

func (m *Manager) waitForL2EndpointIP(ctx context.Context, namespace, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		pod := &corev1.Pod{}
		if err := m.ctrl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return "", fmt.Errorf("get l2 endpoint pod: %w", err)
		}
		if pod.Status.PodIP != "" {
			return pod.Status.PodIP, nil
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for l2 endpoint pod IP: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type l2EndpointPodInput struct {
	AllocationID string
	Namespace    string
	Name         string
	Image        string
	Network      string
	L2TunnelJSON string
	ExpiresAt    time.Time
}

func buildL2EndpointPod(in l2EndpointPodInput) *corev1.Pod {
	privileged := true
	addCaps := []corev1.Capability{"NET_ADMIN"}
	runAsRoot := int64(0)
	zero := int64(0)

	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: in.Namespace,
			Labels:    ownedLabels(in.AllocationID, "l2-endpoint"),
			Annotations: map[string]string{
				labels.ExpiresAtAnnotation: in.ExpiresAt.Format(time.RFC3339),
				labels.L2TunnelAnnotation:  in.L2TunnelJSON,
				"k8s.v1.cni.cncf.io/networks": in.Network,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &zero,
			Containers: []corev1.Container{{
				Name:  "l2-endpoint",
				Image: in.Image,
				Args:  []string{"l2-endpoint"},
				Env: []corev1.EnvVar{{
					Name:  "PLAYPEN_L2_TUNNEL",
					Value: in.L2TunnelJSON,
				}},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged,
					RunAsUser:  &runAsRoot,
					Capabilities: &corev1.Capabilities{
						Add: addCaps,
					},
				},
			}},
		},
	}
}

func applyBootSpec(vm *kubevirtv1.VirtualMachine, cfg BootConfig, httpBootImage string) {
	rootOrder := uint(1)
	networkOrder := uint(2)
	httpOrder := uint(3)

	switch cfg.Target {
	case BootTargetPxe:
		rootOrder = 2
		networkOrder = 1
	case BootTargetUefiHTTP:
		rootOrder = 2
		networkOrder = 1
		if httpBootImage != "" {
			rootOrder = 3
			networkOrder = 2
			httpOrder = 1
		}
	default:
		rootOrder = 1
		networkOrder = 2
	}

	disks := []kubevirtv1.Disk{
		{Name: "rootdisk", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio}}, BootOrder: ptr.To(rootOrder)},
		{Name: "cloudinitdisk", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio}}},
	}
	if httpBootImage != "" {
		disks = append(disks, kubevirtv1.Disk{Name: "httpbootdisk", DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA}}, BootOrder: ptr.To(httpOrder)})
	}

	if vm.Spec.Template == nil {
		vm.Spec.Template = &kubevirtv1.VirtualMachineInstanceTemplateSpec{}
	}
	vm.Spec.Template.Spec.Domain.Devices.Disks = disks
	vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{{
		Name:                   "playpen",
		InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Bridge: &kubevirtv1.InterfaceBridge{}},
		BootOrder:              ptr.To(networkOrder),
	}}
}

func (m *Manager) consumeBootOnce(ctx context.Context, allocationID string) error {
	cfg, err := m.BootConfig(ctx, allocationID)
	if err != nil {
		return err
	}
	if cfg.Enabled != BootOnce {
		return nil
	}

	return m.SetBootConfig(ctx, allocationID, BootConfig{Target: BootTargetHdd, Enabled: BootDisabled})
}

func ownedLabels(allocationID, component string) map[string]string {
	return map[string]string{
		labels.ManagedByLabel:    labels.AppName,
		labels.ComponentLabel:    component,
		labels.AllocationIDLabel: allocationID,
		labels.OwnedLabel:        "true",
	}
}

func newAllocationID(prefix string) string {
	prefix = dnsLabel(prefix)
	if prefix == "" {
		prefix = "pp"
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	maxPrefixLen := 63 - len(suffix) - 1
	if len(prefix) > maxPrefixLen {
		prefix = strings.Trim(prefix[:maxPrefixLen], "-")
	}
	if prefix == "" {
		return suffix
	}

	return prefix + "-" + suffix
}

func dnsLabel(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}

	return out
}

func macForAllocation(allocationID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(allocationID)) //nolint:errcheck
	sum := h.Sum32()

	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
}

func podCIDRForAllocation(base string, allocationID string) string {
	_, ipnet, err := net.ParseCIDR(base)
	if err != nil || ipnet == nil || ipnet.IP.To4() == nil {
		base = "10.241.0.0/16"
		_, ipnet, _ = net.ParseCIDR(base)
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(allocationID)) //nolint:errcheck
	third := byte(h.Sum32() % 250)
	ip := ipnet.IP.To4()

	return fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], third)
}

func cloudInit(sshKey string) string {
	parts := []string{"#cloud-config", "users:", "- name: playpen", "  sudo: ALL=(ALL) NOPASSWD:ALL", "  shell: /bin/bash"}
	if strings.TrimSpace(sshKey) != "" {
		parts = append(parts, "  ssh_authorized_keys:", "  - "+sshKey)
	}

	return strings.Join(parts, "\n") + "\n"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
