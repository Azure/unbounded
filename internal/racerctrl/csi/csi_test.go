// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package csi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// fakeAgent stands in for the node agent. It records what the driver asked of
// it and hands back device paths without touching a ublk device.
type fakeAgent struct {
	devices  map[string]string
	device   string
	staged   []string
	unstaged []string
	stageErr error
	nodeID   uint32
	zone     uint32
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{devices: map[string]string{}, device: "/dev/ublkb42", nodeID: 7, zone: 3}
}

func (a *fakeAgent) Stage(_ context.Context, volume string) (string, error) {
	if a.stageErr != nil {
		return "", a.stageErr
	}

	a.staged = append(a.staged, volume)
	device := a.device
	a.devices[volume] = device

	return device, nil
}

func (a *fakeAgent) Unstage(volume string) {
	a.unstaged = append(a.unstaged, volume)
	delete(a.devices, volume)
}

func (a *fakeAgent) DevicePath(volume string) (string, bool) {
	device, ok := a.devices[volume]

	return device, ok
}

func (a *fakeAgent) NodeID() uint32 { return a.nodeID }

func (a *fakeAgent) Zone() uint32 { return a.zone }

// mountCall records one bind or remount the driver attempted.
type mountCall struct {
	source string
	target string
	flags  uintptr
}

// harness is a driver wired to a fake agent and recorded mount syscalls, over a
// directory laid out the way the kubelet lays out its plugin tree.
type harness struct {
	driver   *Driver
	agent    *fakeAgent
	root     string
	mounts   []mountCall
	unmounts []string
	mountErr error
	unmntErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	agent := newFakeAgent()
	h := &harness{
		agent: agent,
		root:  t.TempDir(),
	}

	h.driver = NewDriver("node-a", agent, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.driver.mount = func(source, target string, flags uintptr) error {
		h.mounts = append(h.mounts, mountCall{source: source, target: target, flags: flags})

		return h.mountErr
	}
	h.driver.unmount = func(target string) error {
		h.unmounts = append(h.unmounts, target)

		return h.unmntErr
	}

	return h
}

// stagingPath is where the kubelet stages a block volume, and it creates it as
// a directory before it calls NodeStageVolume.
func (h *harness) stagingPath(t *testing.T, spec string) string {
	t.Helper()

	path := filepath.Join(h.root, "plugins/kubernetes.io/csi/volumeDevices/staging", spec)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("create staging directory: %v", err)
	}

	return path
}

// publishPath is where the kubelet expects to find the device node. It creates
// the parent directory; the driver has to create the leaf itself.
func (h *harness) publishPath(t *testing.T, spec, pod string) string {
	t.Helper()

	dir := filepath.Join(h.root, "plugins/kubernetes.io/csi/volumeDevices/publish", spec)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create publish directory: %v", err)
	}

	return filepath.Join(dir, pod)
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}

	if got := status.Code(err); got != want {
		t.Fatalf("expected code %s, got %s (%v)", want, got, err)
	}
}

// The regression this whole file exists for: the kubelet creates the staging
// path as a directory, so anything the driver writes at that path fails with
// EISDIR and takes every block volume on the node with it.
func TestStageAcceptsTheDirectoryTheKubeletCreates(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")

	_, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	if len(h.agent.staged) != 1 || h.agent.staged[0] != "pv-1" {
		t.Fatalf("expected the agent to be asked to stage pv-1, got %v", h.agent.staged)
	}

	info, err := os.Stat(staging)
	if err != nil {
		t.Fatalf("stat staging path: %v", err)
	}

	if !info.IsDir() {
		t.Fatal("the driver replaced the kubelet's staging directory")
	}
}

func TestStageCreatesTheStagingDirectoryWhenItIsMissing(t *testing.T) {
	h := newHarness(t)
	staging := filepath.Join(h.root, "plugins/kubernetes.io/csi/volumeDevices/staging/pv-2")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-2",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	info, err := os.Stat(staging)
	if err != nil {
		t.Fatalf("stat staging path: %v", err)
	}

	if !info.IsDir() {
		t.Fatal("expected a staging directory")
	}
}

func TestStageIsIdempotent(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-3")

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-3",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}

	for range 3 {
		if _, err := h.driver.NodeStageVolume(t.Context(), req); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}

	if len(h.agent.staged) != 3 {
		t.Fatalf("expected every retry to reach the agent, got %v", h.agent.staged)
	}
}

func TestStageRejectsBadRequests(t *testing.T) {
	filesystem := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
	multiNode := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}

	cases := []struct {
		name string
		req  *csi.NodeStageVolumeRequest
	}{
		{
			name: "no volume id",
			req:  &csi.NodeStageVolumeRequest{StagingTargetPath: "/staging", VolumeCapability: blockCapability()},
		},
		{
			name: "no staging path",
			req:  &csi.NodeStageVolumeRequest{VolumeId: "pv-1", VolumeCapability: blockCapability()},
		},
		{
			name: "no capability",
			req:  &csi.NodeStageVolumeRequest{VolumeId: "pv-1", StagingTargetPath: "/staging"},
		},
		{
			name: "filesystem mode",
			req: &csi.NodeStageVolumeRequest{
				VolumeId: "pv-1", StagingTargetPath: "/staging", VolumeCapability: filesystem,
			},
		},
		{
			name: "multi node access",
			req: &csi.NodeStageVolumeRequest{
				VolumeId: "pv-1", StagingTargetPath: "/staging", VolumeCapability: multiNode,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			_, err := h.driver.NodeStageVolume(t.Context(), tc.req)
			requireCode(t, err, codes.InvalidArgument)

			if len(h.agent.staged) != 0 {
				t.Fatalf("a rejected request reached the agent: %v", h.agent.staged)
			}
		})
	}
}

func TestStageReportsAgentFailure(t *testing.T) {
	h := newHarness(t)
	h.agent.stageErr = errors.New("no device")

	_, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: h.stagingPath(t, "pv-1"),
		VolumeCapability:  blockCapability(),
	})
	requireCode(t, err, codes.Internal)
}

// The kubelet removes the staging directory itself. A driver that removes it
// either fails on a non-empty directory or races the kubelet for no gain.
func TestUnstageLeavesTheStagingDirectoryToTheKubelet(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")

	if _, err := h.driver.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
	}); err != nil {
		t.Fatalf("unstage: %v", err)
	}

	if len(h.agent.unstaged) != 1 || h.agent.unstaged[0] != "pv-1" {
		t.Fatalf("expected the agent to be told to unstage pv-1, got %v", h.agent.unstaged)
	}

	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("the driver removed the kubelet's staging directory: %v", err)
	}
}

func TestUnstageIsIdempotentForAVolumeThatWasNeverStaged(t *testing.T) {
	h := newHarness(t)

	if _, err := h.driver.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pv-unknown",
		StagingTargetPath: filepath.Join(h.root, "gone"),
	}); err != nil {
		t.Fatalf("unstage: %v", err)
	}
}

func TestUnstageRequiresAVolumeID(t *testing.T) {
	h := newHarness(t)

	_, err := h.driver.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{})
	requireCode(t, err, codes.InvalidArgument)
}

func TestPublishBindsTheDeviceOntoTheKubeletsPath(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(h.mounts) != 1 {
		t.Fatalf("expected one bind mount, got %v", h.mounts)
	}

	got := h.mounts[0]
	if got.source != "/dev/ublkb42" || got.target != target {
		t.Fatalf("expected /dev/ublkb42 bound onto %s, got %+v", target, got)
	}

	if got.flags&unix.MS_BIND == 0 {
		t.Fatalf("expected a bind mount, got flags %#x", got.flags)
	}

	if got.flags&unix.MS_RDONLY != 0 {
		t.Fatalf("expected a writable mount, got flags %#x", got.flags)
	}

	// The placeholder the bind mount is laid over has to exist as a file.
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}

	if info.IsDir() {
		t.Fatal("expected a file at the publish target, got a directory")
	}
}

func TestPublishReadOnlyRemounts(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
		Readonly:         true,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(h.mounts) != 2 {
		t.Fatalf("expected a bind and a read-only remount, got %v", h.mounts)
	}

	remount := h.mounts[1]
	if remount.flags&unix.MS_REMOUNT == 0 || remount.flags&unix.MS_RDONLY == 0 {
		t.Fatalf("expected a read-only remount, got flags %#x", remount.flags)
	}
}

// A retried publish finds the bind already in place, which the kernel reports
// as EBUSY. That is success, not failure.
func TestPublishToleratesAnExistingBind(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	h.mountErr = unix.EBUSY

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
	}); err != nil {
		t.Fatalf("expected a retried publish to succeed, got %v", err)
	}
}

// A bind of the same source onto the same target does not fail with EBUSY: it
// succeeds and stacks a second mount on the first. The kubelet retries
// NodePublishVolume freely, so a driver that binds unconditionally builds a
// pile of mounts that NodeUnpublishVolume, which unmounts once, can never take
// back down. The driver has to notice the device is already there.
func TestPublishSkipsABindThatIsAlreadyInPlace(t *testing.T) {
	h := newHarness(t)
	h.agent.device = hostBlockDevice(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// A published target stats as the device itself. A symlink is the only way
	// to arrange that without mknod, and it resolves to the same device
	// number, which is all the driver looks at.
	if err := os.Symlink(h.agent.device, target); err != nil {
		t.Fatalf("stand in for a published target: %v", err)
	}

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(h.mounts) != 0 {
		t.Fatalf("expected no second bind onto a published target, got %v", h.mounts)
	}
}

// A target that holds something other than the volume's device is not a
// published target, whatever else it is.
func TestPublishBindsOverATargetHoldingSomethingElse(t *testing.T) {
	h := newHarness(t)
	h.agent.device = hostBlockDevice(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if err := os.Symlink("/dev/null", target); err != nil {
		t.Fatalf("stand in for a foreign target: %v", err)
	}

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(h.mounts) != 1 {
		t.Fatalf("expected one bind over the foreign target, got %v", h.mounts)
	}
}

func TestBoundToReadsAnUnpublishedTarget(t *testing.T) {
	device := hostBlockDevice(t)
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing")
	if bound, err := boundTo(device, missing); err != nil || bound {
		t.Fatalf("expected a missing target to be unbound, got %v (%v)", bound, err)
	}

	placeholder := filepath.Join(dir, "placeholder")
	if err := os.WriteFile(placeholder, nil, 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	if bound, err := boundTo(device, placeholder); err != nil || bound {
		t.Fatalf("expected a placeholder to be unbound, got %v (%v)", bound, err)
	}

	// A device that has gone away cannot be bound anywhere, and saying so is
	// more useful than failing the publish on a stat.
	if bound, err := boundTo(filepath.Join(dir, "gone"), placeholder); err != nil || bound {
		t.Fatalf("expected a missing device to be unbound, got %v (%v)", bound, err)
	}
}

// hostBlockDevice names a block device that exists on the machine running the
// test, so the driver's device-number comparison has something real to read.
func hostBlockDevice(t *testing.T) string {
	t.Helper()

	names, err := os.ReadDir("/sys/block")
	if err != nil {
		t.Skipf("no block devices to read: %v", err)
	}

	for _, name := range names {
		path := filepath.Join("/dev", name.Name())

		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			continue
		}

		if st.Mode&unix.S_IFMT == unix.S_IFBLK {
			return path
		}
	}

	t.Skip("no block device node to compare against")

	return ""
}

func TestPublishReportsAMountFailure(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	h.mountErr = unix.EPERM

	_, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
	})
	requireCode(t, err, codes.Internal)
}

func TestPublishRefusesAVolumeThatIsNotStaged(t *testing.T) {
	h := newHarness(t)

	_, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       h.publishPath(t, "pv-1", "pod-uid"),
		VolumeCapability: blockCapability(),
	})
	requireCode(t, err, codes.FailedPrecondition)

	if len(h.mounts) != 0 {
		t.Fatalf("expected no mount attempt, got %v", h.mounts)
	}
}

func TestPublishRejectsBadRequests(t *testing.T) {
	h := newHarness(t)

	_, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		TargetPath:       "/target",
		VolumeCapability: blockCapability(),
	})
	requireCode(t, err, codes.InvalidArgument)

	_, err = h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		VolumeCapability: blockCapability(),
	})
	requireCode(t, err, codes.InvalidArgument)
}

func TestUnpublishUnmountsAndRemovesTheTarget(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	if _, err := h.driver.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if _, err := h.driver.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pv-1",
		TargetPath:       target,
		VolumeCapability: blockCapability(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := h.driver.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pv-1",
		TargetPath: target,
	}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}

	if len(h.unmounts) != 1 || h.unmounts[0] != target {
		t.Fatalf("expected %s to be unmounted, got %v", target, h.unmounts)
	}

	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected the target to be gone, got %v", err)
	}
}

// EINVAL means nothing was mounted there, which is what a retried unpublish
// looks like.
func TestUnpublishIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.unmntErr = unix.EINVAL

	if _, err := h.driver.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pv-1",
		TargetPath: filepath.Join(h.root, "never-published"),
	}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
}

func TestUnpublishReportsAnUnmountFailure(t *testing.T) {
	h := newHarness(t)
	h.unmntErr = unix.EPERM

	_, err := h.driver.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pv-1",
		TargetPath: filepath.Join(h.root, "busy"),
	})
	requireCode(t, err, codes.Internal)
}

func TestUnpublishRequiresATargetPath(t *testing.T) {
	h := newHarness(t)

	_, err := h.driver.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{VolumeId: "pv-1"})
	requireCode(t, err, codes.InvalidArgument)
}

// The full sequence the kubelet drives, over paths shaped the way it shapes
// them, twice: a volume that is staged, published, unpublished, unstaged and
// then staged again has to come back.
func TestTheKubeletSequenceRoundTrips(t *testing.T) {
	h := newHarness(t)
	staging := h.stagingPath(t, "pv-1")
	target := h.publishPath(t, "pv-1", "pod-uid")

	stage := &csi.NodeStageVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		VolumeCapability:  blockCapability(),
	}
	publish := &csi.NodePublishVolumeRequest{
		VolumeId:          "pv-1",
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	}

	for range 2 {
		if _, err := h.driver.NodeStageVolume(t.Context(), stage); err != nil {
			t.Fatalf("stage: %v", err)
		}

		if _, err := h.driver.NodePublishVolume(t.Context(), publish); err != nil {
			t.Fatalf("publish: %v", err)
		}

		if _, err := h.driver.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
			VolumeId:   "pv-1",
			TargetPath: target,
		}); err != nil {
			t.Fatalf("unpublish: %v", err)
		}

		if _, err := h.driver.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
			VolumeId:          "pv-1",
			StagingTargetPath: staging,
		}); err != nil {
			t.Fatalf("unstage: %v", err)
		}
	}

	if len(h.agent.staged) != 2 || len(h.agent.unstaged) != 2 {
		t.Fatalf("expected two full cycles, staged %v unstaged %v", h.agent.staged, h.agent.unstaged)
	}
}

func TestNodeGetInfoReportsTheZoneAndTheExportBudget(t *testing.T) {
	h := newHarness(t)

	info, err := h.driver.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("node get info: %v", err)
	}

	if info.GetNodeId() != "node-a" {
		t.Fatalf("expected node-a, got %s", info.GetNodeId())
	}

	if info.GetMaxVolumesPerNode() != racerctrl.MaxExports/2 {
		t.Fatalf("expected %d volumes, got %d", racerctrl.MaxExports/2, info.GetMaxVolumesPerNode())
	}

	if got := info.GetAccessibleTopology().GetSegments()[TopologyZoneKey]; got != "3" {
		t.Fatalf("expected zone 3 in the topology, got %q", got)
	}
}

// A node the operator has not placed yet has no zone, and reporting zone 0
// would tell the scheduler something false.
func TestNodeGetInfoOmitsAnUnassignedZone(t *testing.T) {
	h := newHarness(t)
	h.agent.zone = 0

	info, err := h.driver.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("node get info: %v", err)
	}

	if info.GetAccessibleTopology() != nil {
		t.Fatalf("expected no topology, got %v", info.GetAccessibleTopology())
	}
}

func TestProbeWaitsForAnIdentity(t *testing.T) {
	h := newHarness(t)
	h.agent.nodeID = 0

	resp, err := h.driver.Probe(t.Context(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if resp.GetReady().GetValue() {
		t.Fatal("expected a node without an id to report not ready")
	}

	h.agent.nodeID = 9

	resp, err = h.driver.Probe(t.Context(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if !resp.GetReady().GetValue() {
		t.Fatal("expected a placed node to report ready")
	}
}

func TestNodeGetCapabilitiesClaimsStaging(t *testing.T) {
	h := newHarness(t)

	resp, err := h.driver.NodeGetCapabilities(t.Context(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("node get capabilities: %v", err)
	}

	var staging bool

	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
			staging = true
		}
	}

	if !staging {
		t.Fatalf("expected STAGE_UNSTAGE_VOLUME, got %v", resp.GetCapabilities())
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		in      string
		network string
		address string
		wantErr bool
	}{
		{in: "/csi/csi.sock", network: "unix", address: "/csi/csi.sock"},
		{in: "unix:///csi/csi.sock", network: "unix", address: "/csi/csi.sock"},
		{in: "tcp://127.0.0.1:9000", network: "tcp", address: "127.0.0.1:9000"},
		{in: "http://example", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			network, address, err := parseEndpoint(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if network != tc.network || address != tc.address {
				t.Fatalf("expected %s/%s, got %s/%s", tc.network, tc.address, network, address)
			}
		})
	}
}
