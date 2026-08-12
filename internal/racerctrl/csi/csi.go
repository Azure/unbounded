// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package csi serves the CSI Identity and Node services for racer volumes.
//
// There is no Controller service and no external-provisioner sidecar. A racer
// volume is not something a provisioner can create on its own: its extents are
// allocated out of a cluster-wide identifier space, placed in a universe's
// address space, and replicated by a catalog whose membership is a sequenced
// operation. All of that already belongs to the unbounded-operator, which is
// the single writer of the cluster-scoped state, so a CreateVolume RPC would
// only be able to forward the request to it and wait. Instead the operator
// stamps the allocation directly onto the PersistentVolume, and this node
// service does the one thing that genuinely has to happen on the node: turn an
// allocated volume into a block device the kubelet can bind into a pod.
//
// Volumes are raw block only. There is no filesystem mode, no mkfs and no
// mount-utils dependency: racer exports a block device and this driver hands
// that device to the pod.
package csi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Azure/unbounded/internal/racerctrl"
	"github.com/Azure/unbounded/internal/version"
)

// TopologyZoneKey is the topology key the driver reports.
//
// A racer volume's extents are homed in one zone, and a pod can only reach them
// from a node that either lives in that zone or can route to it through a
// gateway. Reporting the zone lets the scheduler keep a pod near its data
// instead of discovering the problem at stage time.
const TopologyZoneKey = "racer.unbounded-cloud.io/zone"

// Stager is the part of the node agent the CSI layer needs: it turns a volume
// name into a local block device and takes it away again.
type Stager interface {
	// Stage exports the volume on a local ublk device and returns its path.
	Stage(ctx context.Context, volume string) (string, error)

	// Unstage stops exporting the volume.
	Unstage(volume string)

	// DevicePath reports the device a volume is currently exported on.
	DevicePath(volume string) (string, bool)

	// NodeID is this node's racer id, zero until the operator admits it.
	NodeID() uint32

	// Zone is this node's racer zone, zero until the operator admits it.
	Zone() uint32
}

// Driver serves the Identity and Node services.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	nodeName string
	agent    Stager
	log      *slog.Logger

	// mu serializes stage and publish for a single volume. The CSI spec allows
	// the kubelet to issue calls for different volumes concurrently but
	// requires the plugin to be safe under retries of the same one, and both
	// operations here mutate device bindings.
	mu sync.Mutex
}

// NewDriver builds the driver.
func NewDriver(nodeName string, agent Stager, log *slog.Logger) *Driver {
	return &Driver{nodeName: nodeName, agent: agent, log: log}
}

// GetPluginInfo identifies the driver.
func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          racerctrl.DriverName,
		VendorVersion: version.Version,
	}, nil
}

// GetPluginCapabilities reports what the plugin can do.
//
// It claims neither CONTROLLER_SERVICE nor VOLUME_ACCESSIBILITY_CONSTRAINTS
// beyond the node topology it publishes: there is no controller, and volume
// placement is decided by the operator when it allocates extents rather than
// negotiated through the CSI topology mechanism.
func (d *Driver) GetPluginCapabilities(
	context.Context, *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

// Probe reports readiness. The driver is ready once the operator has admitted
// this node, because until then there is no identity to render a config under
// and any stage would time out.
func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{
		Ready: wrapperspb.Bool(d.agent.NodeID() != 0),
	}, nil
}

// NodeGetInfo identifies this node to the kubelet.
func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	info := &csi.NodeGetInfoResponse{
		NodeId: d.nodeName,

		// racer exports at most 256 ublk devices per node, and one of those is
		// spent on the fabric device of every universe the node joins. Half the
		// budget is a deliberately conservative volume limit that leaves room
		// for a node to join every universe in a large cluster without the
		// kubelet ever scheduling a pod whose volume cannot be exported.
		MaxVolumesPerNode: racerctrl.MaxExports / 2,
	}

	if zone := d.agent.Zone(); zone != 0 {
		info.AccessibleTopology = &csi.Topology{
			Segments: map[string]string{TopologyZoneKey: fmt.Sprint(zone)},
		}
	}

	return info, nil
}

// NodeGetCapabilities reports the node service capabilities.
//
// STAGE_UNSTAGE_VOLUME is the whole point: staging is where the volume acquires
// a local ublk minor and racer is told to export it, and that must happen once
// per node rather than once per pod. Notably absent is EXPAND_VOLUME. A racer
// extent's size is frozen for its life, because the address space around it has
// already been allocated to other extents and the slot hash that decides which
// group owns a page is a function of the address; growing an extent would move
// pages between groups. Immutability is not a limitation of this driver, it is
// a property of the address space.
func (d *Driver) NodeGetCapabilities(
	context.Context, *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}

// NodeStageVolume exports the volume as a local block device.
//
// The staging target path is not used as a mount point: there is nothing to
// mount, since the volume is raw block. It is used as a marker file holding the
// device path, so an interrupted publish can be retried and a stale stage can
// be recognised after a plugin restart.
func (d *Driver) NodeStageVolume(
	ctx context.Context, req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	if err := checkBlockCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	device, err := d.agent.Stage(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stage volume %s: %v", req.GetVolumeId(), err)
	}

	if err := os.MkdirAll(filepath.Dir(req.GetStagingTargetPath()), 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create staging directory: %v", err)
	}

	// 0600 root:root, like the device node itself. Anything that can read this
	// file learns which minor carries which volume, which is a small but free
	// piece of information to withhold.
	if err := os.WriteFile(req.GetStagingTargetPath(), []byte(device), 0o600); err != nil {
		return nil, status.Errorf(codes.Internal, "record staged device: %v", err)
	}

	d.log.Info("staged volume", "volume", req.GetVolumeId(), "device", device)

	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume stops exporting the volume.
func (d *Driver) NodeUnstageVolume(
	_ context.Context, req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.agent.Unstage(req.GetVolumeId())

	if path := req.GetStagingTargetPath(); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Internal, "remove staging marker: %v", err)
		}
	}

	d.log.Info("unstaged volume", "volume", req.GetVolumeId())

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume makes the staged device visible inside the pod.
//
// For a raw block volume the kubelet supplies a path at which it expects to
// find a device node, and the driver bind-mounts the real device onto it. The
// bind mount is what keeps the exposure narrow: the pod sees one device node
// for one volume, not the /dev directory that holds every other volume on the
// node.
func (d *Driver) NodePublishVolume(
	_ context.Context, req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if err := checkBlockCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	device, ok := d.agent.DevicePath(req.GetVolumeId())
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is not staged on this node", req.GetVolumeId())
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create target directory: %v", err)
	}

	// The bind mount source must exist as a file at the target. An empty
	// regular file is the conventional placeholder for a block bind mount.
	if err := ensurePlaceholder(target); err != nil {
		return nil, status.Errorf(codes.Internal, "prepare target %s: %v", target, err)
	}

	flags := uintptr(unix.MS_BIND)
	if req.GetReadonly() {
		flags |= unix.MS_RDONLY
	}

	if err := unix.Mount(device, target, "", flags, ""); err != nil {
		if !errors.Is(err, unix.EBUSY) {
			return nil, status.Errorf(codes.Internal, "bind %s onto %s: %v", device, target, err)
		}
		// EBUSY means the bind is already in place, which is what a retried
		// publish looks like.
	}

	if req.GetReadonly() {
		// MS_RDONLY is ignored on the initial bind and has to be applied by a
		// second remount call.
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "remount %s read-only: %v", target, err)
		}
	}

	d.log.Info("published volume", "volume", req.GetVolumeId(), "device", device, "target", target)

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume removes the bind mount.
func (d *Driver) NodeUnpublishVolume(
	_ context.Context, req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := unix.Unmount(target, 0); err != nil &&
		!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
	}

	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", target, err)
	}

	d.log.Info("unpublished volume", "volume", req.GetVolumeId(), "target", target)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats is not implemented: racer reports per-extent liveness
// through its own metrics endpoint, which carries far more than the used and
// available byte counts this RPC can express, and there is no meaningful
// "available" figure for an extent whose size is fixed.
func (d *Driver) NodeGetVolumeStats(
	context.Context, *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "racer does not report per-volume statistics through CSI")
}

// checkBlockCapability refuses anything but raw block access.
func checkBlockCapability(capability *csi.VolumeCapability) error {
	if capability == nil {
		return status.Error(codes.InvalidArgument, "volume capability is required")
	}

	if capability.GetBlock() == nil {
		return status.Error(codes.InvalidArgument,
			"racer volumes are raw block only: set volumeMode: Block on the PersistentVolumeClaim")
	}

	switch capability.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		return nil
	default:
		// A racer volume can be exported from several nodes at once and the
		// replication protocol makes that coherent, but the exported device
		// advertises no multi-writer semantics to the filesystem above it and
		// the kubelet would be entitled to assume otherwise. Multi-node access
		// modes are refused until that story is finished rather than accepted
		// and quietly mis-served.
		return status.Errorf(codes.InvalidArgument,
			"access mode %s is not supported: racer volumes are single-node",
			capability.GetAccessMode().GetMode())
	}
}

// ensurePlaceholder creates the empty file a block bind mount is laid over.
func ensurePlaceholder(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory; expected a block target", path)
		}

		return nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	return f.Close()
}

// Serve runs the gRPC server on the CSI endpoint until the context is
// cancelled.
func Serve(ctx context.Context, endpoint string, driver *Driver, log *slog.Logger) error {
	network, address, err := parseEndpoint(endpoint)
	if err != nil {
		return err
	}

	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(address), 0o750); err != nil {
			return fmt.Errorf("create socket directory: %w", err)
		}

		// A socket left behind by a killed plugin would make Listen fail with
		// EADDRINUSE even though nothing is listening.
		if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket %s: %w", address, err)
		}
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", endpoint, err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(logInterceptor(log)))

	csi.RegisterIdentityServer(server, driver)
	csi.RegisterNodeServer(server, driver)

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	log.Info("serving CSI", "endpoint", endpoint)

	if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve CSI: %w", err)
	}

	return nil
}

// logInterceptor records failed RPCs. Successful ones are left silent: the
// kubelet probes the identity service continuously and logging every call would
// bury everything else.
func logInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			log.Error("CSI call failed", "method", info.FullMethod, "error", err)
		}

		return resp, err
	}
}

func parseEndpoint(endpoint string) (string, string, error) {
	if strings.HasPrefix(endpoint, "/") {
		return "unix", endpoint, nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("parse CSI endpoint %q: %w", endpoint, err)
	}

	switch u.Scheme {
	case "unix":
		return "unix", filepath.Join(u.Host, u.Path), nil
	case "tcp":
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported CSI endpoint scheme %q", u.Scheme)
	}
}
