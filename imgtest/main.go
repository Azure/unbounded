// Command memsnap serves a containerd proxy snapshotter with immutable image
// layers on RACER and writable container layers on the local filesystem.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const (
	sectorSize    = 512
	dataBlockSize = 128 // 64 KiB thin-pool allocation blocks.
	lowWaterMark  = 1024
	imageRefLabel = "containerd.io/snapshot.ref"
	roleLabel     = "memsnap.io/storage-role"
	imageRole     = "image"
	localRole     = "local"
)

type config struct {
	Root       string
	PoolName   string
	Device     string
	MetaSize   int64
	VolumeSize int64
}

func defaultConfig() config {
	const gib = 1 << 30
	return config{
		Root:       "/var/lib/memsnap",
		PoolName:   "memsnap-pool",
		MetaSize:   64 << 20,
		VolumeSize: 2 * gib,
	}
}

func main() {
	cfg := defaultConfig()
	address := flag.String("address", "/run/memsnap/memsnap.sock", "unix socket to serve the snapshotter API on")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.StringVar(&cfg.Root, "root", cfg.Root, "directory for persistent metadata and container writable layers")
	flag.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "device-mapper thin pool name")
	flag.StringVar(&cfg.Device, "device", cfg.Device, "RACER block device used only for image data")
	flag.Int64Var(&cfg.MetaSize, "meta-size", cfg.MetaSize, "size of the local thin-pool metadata file")
	flag.Int64Var(&cfg.VolumeSize, "volume-size", cfg.VolumeSize, "virtual size of each container filesystem in bytes")
	flag.Parse()

	if *debug {
		_ = log.SetLevel("debug")
	}
	if cfg.Device == "" {
		log.L.Fatal("-device must name the RACER block device")
	}
	if err := run(cfg, *address); err != nil {
		log.L.WithError(err).Fatal("memsnap failed")
	}
}

func run(cfg config, address string) error {
	sn, err := newSnapshotter(cfg)
	if err != nil {
		return err
	}
	defer sn.Close()

	listener, err := listen(address)
	if err != nil {
		return err
	}
	defer listener.Close()

	server := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(server, snapshotservice.FromSnapshotter(sn))

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		sig := <-signals
		log.L.WithField("signal", sig).Info("shutting down")
		server.Stop()
	}()

	log.L.WithField("address", address).Info("serving snapshotter")
	return server.Serve(listener)
}

func listen(address string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		return nil, err
	}
	if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return net.Listen("unix", address)
}

// Device-mapper helpers.

func dmsetup(args ...string) (string, error) {
	cmd := exec.Command("dmsetup", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dmsetup %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func devicePath(name string) string { return "/dev/mapper/" + name }

func deviceExists(name string) bool {
	_, err := dmsetup("info", "--noheadings", "-c", "-o", "name", name)
	return err == nil
}

func removeDevice(name string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		if !deviceExists(name) {
			return nil
		}
		if _, err = dmsetup("remove", name); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func listDevices(prefix string) ([]string, error) {
	var names []string
	for _, target := range []string{"thin", "thin-pool"} {
		out, err := dmsetup("ls", "--target", target)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] != "No" && strings.HasPrefix(fields[0], prefix) {
				names = append(names, fields[0])
			}
		}
	}
	return names, nil
}

func sweep(poolName string) error {
	names, err := listDevices(poolName)
	if err != nil {
		return err
	}
	for _, name := range names {
		log.L.WithField("device", name).Warn("removing stale device")
		if err := removeDevice(name); err != nil {
			return fmt.Errorf("removing stale device %s: %w", name, err)
		}
	}
	return nil
}

func blockDeviceSectors(device string) (int64, error) {
	out, err := exec.Command("blockdev", "--getsz", device).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsz %s: %w: %s", device, err, strings.TrimSpace(string(out)))
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size of %s: %w", device, err)
	}
	return sectors, nil
}

func createDataDevice(poolName, device string) (name string, dataBytes int64, err error) {
	totalSectors, err := blockDeviceSectors(device)
	if err != nil {
		return "", 0, err
	}
	if totalSectors == 0 {
		return "", 0, fmt.Errorf("RACER device %s is empty", device)
	}

	name = poolName + "-data"
	if _, err := dmsetup("create", name, "--table", fmt.Sprintf("0 %d linear %s 0", totalSectors, device)); err != nil {
		return "", 0, err
	}
	return name, totalSectors * sectorSize, nil
}

func localMetadataDevice(path string, size int64) (device string, err error) {
	if size < 4<<20 {
		return "", fmt.Errorf("metadata size %d is too small", size)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err == nil && info.Size() != 0 && info.Size() != size {
		err = fmt.Errorf("local thin metadata %s is %d bytes, expected %d", path, info.Size(), size)
	}
	if err == nil {
		err = unix.Fallocate(int(f.Fd()), 0, 0, size)
	}
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("creating local thin metadata: %w", err)
	}
	if out, err := exec.Command("losetup", "--associated", path, "--output", "NAME", "--noheadings").CombinedOutput(); err == nil {
		for _, stale := range strings.Fields(string(out)) {
			if detachOut, err := exec.Command("losetup", "--detach", stale).CombinedOutput(); err != nil {
				return "", fmt.Errorf("detaching stale %s: %w: %s", stale, err, strings.TrimSpace(string(detachOut)))
			}
		}
	}
	out, err := exec.Command("losetup", "--find", "--show", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("losetup %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

type pool struct {
	Name string
}

func newPool(name, metadataDevice, dataDevice string, dataSize int64) (*pool, error) {
	// Runtime image deletion changes only local thin metadata. In particular,
	// ignore_discard prevents it from issuing writes or discards to RACER.
	table := fmt.Sprintf("0 %d thin-pool %s %s %d %d 1 ignore_discard", dataSize/sectorSize, metadataDevice, dataDevice, dataBlockSize, lowWaterMark)
	if _, err := dmsetup("create", name, "--table", table); err != nil {
		return nil, err
	}
	return &pool{Name: name}, nil
}

func (p *pool) message(format string, args ...any) error {
	_, err := dmsetup("message", devicePath(p.Name), "0", fmt.Sprintf(format, args...))
	return err
}

func (p *pool) createVolume(id uint64) error { return p.message("create_thin %d", id) }

func (p *pool) createSnapshot(id, originID uint64) error {
	return p.message("create_snap %d %d", id, originID)
}

func (p *pool) deleteVolume(id uint64) error { return p.message("delete %d", id) }

func (p *pool) activate(name string, id uint64, virtualSize int64, readonly bool) error {
	table := fmt.Sprintf("0 %d thin %s %d", virtualSize/sectorSize, devicePath(p.Name), id)
	args := []string{"create", name, "--table", table}
	if readonly {
		args = append(args, "--readonly")
	}
	_, err := dmsetup(args...)
	return err
}

func mappedBytes(name string) (int64, error) {
	out, err := dmsetup("status", name)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(out)
	if len(fields) < 4 || fields[2] != "thin" {
		return 0, fmt.Errorf("unexpected thin status %q", out)
	}
	sectors, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse mapped sectors in %q: %w", out, err)
	}
	return sectors * sectorSize, nil
}

// Snapshotter implementation.

type snapshotter struct {
	cfg            config
	ms             *storage.MetaStore
	pool           *pool
	metadataDevice string // Local loop device, never RACER.
	dataDevice     string
	closeOnce      sync.Once
}

var _ snapshots.Snapshotter = (*snapshotter)(nil)

func newSnapshotter(cfg config) (_ *snapshotter, err error) {
	if err := unmountLocalLowers(cfg.Root); err != nil {
		return nil, fmt.Errorf("unmounting stale local layers: %w", err)
	}
	if err := sweep(cfg.PoolName); err != nil {
		return nil, fmt.Errorf("sweeping stale devices: %w", err)
	}
	for _, name := range []string{cfg.PoolName + "-data"} {
		if err := removeDevice(name); err != nil {
			return nil, fmt.Errorf("removing stale device %s: %w", name, err)
		}
	}
	if err := os.MkdirAll(cfg.Root, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root, "snapshots"), 0700); err != nil {
		return nil, err
	}

	s := &snapshotter{cfg: cfg}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	s.metadataDevice, err = localMetadataDevice(filepath.Join(cfg.Root, "thin-metadata"), cfg.MetaSize)
	if err != nil {
		return nil, err
	}
	var dataSize int64
	s.dataDevice, dataSize, err = createDataDevice(cfg.PoolName, cfg.Device)
	if err != nil {
		return nil, err
	}
	s.pool, err = newPool(cfg.PoolName, s.metadataDevice, devicePath(s.dataDevice), dataSize)
	if err != nil {
		return nil, err
	}
	s.ms, err = storage.NewMetaStore(filepath.Join(cfg.Root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	log.L.WithField("pool", cfg.PoolName).WithField("racer_image_device", cfg.Device).
		WithField("local_writes", cfg.Root).WithField("data_bytes", dataSize).
		Info("immutable image pool and local writable store ready")
	return s, nil
}

func (s *snapshotter) deviceName(id string) string { return s.cfg.PoolName + "-" + id }

func devID(id string) (uint64, error) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid snapshot id %q: %w", id, err)
	}
	return n, nil
}

func (s *snapshotter) snapshotPath(id string) string {
	return filepath.Join(s.cfg.Root, "snapshots", id)
}

func (s *snapshotter) localMounts(info storage.Snapshot) []mount.Mount {
	root := s.snapshotPath(info.ID)
	return []mount.Mount{{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			"lowerdir=" + filepath.Join(root, "lower"),
			"upperdir=" + filepath.Join(root, "upper"),
			"workdir=" + filepath.Join(root, "work"),
		},
	}}
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) ([]mount.Mount, error) {
	var mounts []mount.Mount
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) (err error) {
		var requested snapshots.Info
		for _, opt := range opts {
			if err := opt(&requested); err != nil {
				return err
			}
		}
		_, imageUnpack := requested.Labels[imageRefLabel]
		role := localRole
		if imageUnpack {
			role = imageRole
		}
		opts = append(opts, snapshots.WithLabels(map[string]string{roleLabel: role}))
		snap, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return err
		}
		id, err := devID(snap.ID)
		if err != nil {
			return err
		}

		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		imageUnpack = info.Labels[roleLabel] == imageRole
		if imageUnpack && kind != snapshots.KindActive {
			return fmt.Errorf("image snapshot %q must be active", key)
		}

		if imageUnpack && len(snap.ParentIDs) == 0 {
			err = s.pool.createVolume(id)
		} else if imageUnpack {
			var parentID uint64
			if parentID, err = devID(snap.ParentIDs[0]); err == nil {
				err = s.pool.createSnapshot(id, parentID)
			}
		} else if len(snap.ParentIDs) == 0 {
			return fmt.Errorf("container snapshot %q requires an image parent", key)
		}
		if err != nil {
			return err
		}
		defer func() {
			if err != nil && imageUnpack {
				_ = s.pool.deleteVolume(id)
			}
		}()

		name := s.deviceName(snap.ID)
		volumeID := id
		if !imageUnpack {
			volumeID, err = devID(snap.ParentIDs[0])
			if err != nil {
				return err
			}
		}
		if err = s.pool.activate(name, volumeID, s.cfg.VolumeSize, !imageUnpack); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				if !imageUnpack {
					_ = mount.UnmountAll(filepath.Join(s.snapshotPath(snap.ID), "lower"), unix.MNT_DETACH)
					_ = os.RemoveAll(s.snapshotPath(snap.ID))
				}
				_ = removeDevice(name)
			}
		}()

		if imageUnpack && len(snap.ParentIDs) == 0 {
			if err = mkfsExt4(devicePath(name)); err != nil {
				return err
			}
		}
		if imageUnpack {
			mounts = []mount.Mount{{Source: devicePath(name), Type: "ext4", Options: []string{"rw"}}}
		} else if kind == snapshots.KindView {
			mounts = []mount.Mount{{Source: devicePath(name), Type: "ext4", Options: []string{"ro", "noload"}}}
		} else {
			if err = s.prepareLocalLayer(snap); err != nil {
				return err
			}
			mounts = s.localMounts(snap)
		}
		return nil
	})
	return mounts, err
}

func (s *snapshotter) prepareLocalLayer(info storage.Snapshot) error {
	root := s.snapshotPath(info.ID)
	for _, name := range []string{"lower", "upper", "work"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	lower := filepath.Join(root, "lower")
	mounted, err := mountinfo.Mounted(lower)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	m := mount.Mount{Source: devicePath(s.deviceName(info.ID)), Type: "ext4", Options: []string{"ro", "noload"}}
	if err := m.Mount(lower); err != nil {
		return fmt.Errorf("mounting immutable image lower layer: %w", err)
	}
	return nil
}

func unmountLocalLowers(root string) error {
	paths, err := filepath.Glob(filepath.Join(root, "snapshots", "*", "lower"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := mount.UnmountAll(path, unix.MNT_DETACH); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

var mkfsOptions = []string{
	"-F", "-q",
	"-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0",
	"-i", "65536",
	"-J", "size=32",
}

func mkfsExt4(device string) error {
	args := append(slices.Clone(mkfsOptions), device)
	out, err := exec.Command("mkfs.ext4", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", device, err, out)
	}
	return nil
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		snap, err := storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("getting active snapshot %q: %w", key, err)
		}
		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		if info.Labels[roleLabel] != imageRole {
			return fmt.Errorf("committing container writable layer %q is unsupported", key)
		}
		device := s.deviceName(snap.ID)
		var usage snapshots.Usage
		if mapped, err := mappedBytes(device); err == nil {
			usage.Size = mapped
		}
		if err := removeDevice(device); err != nil {
			return fmt.Errorf("deactivating %s: %w", device, err)
		}
		opts = append(opts, snapshots.WithLabels(map[string]string{roleLabel: imageRole}))
		if _, err := storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
			return fmt.Errorf("committing %q: %w", key, err)
		}
		return nil
	})
}

func (s *snapshotter) Remove(ctx context.Context, key string) error {
	var localPath string
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		image := info.Labels[roleLabel] == imageRole || info.Kind == snapshots.KindCommitted
		if !image && info.Kind != snapshots.KindCommitted {
			localPath = s.snapshotPath(id)
			if err := mount.UnmountAll(filepath.Join(localPath, "lower"), unix.MNT_DETACH); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if info.Kind != snapshots.KindCommitted {
			if err := removeDevice(s.deviceName(id)); err != nil {
				return err
			}
		}

		id, _, err = storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("removing snapshot %q: %w", key, err)
		}
		if !image {
			return nil
		}
		deviceID, err := devID(id)
		if err != nil {
			return err
		}
		return s.pool.deleteVolume(deviceID)
	})
	if err != nil {
		return err
	}
	if localPath != "" {
		return os.RemoveAll(localPath)
	}
	return nil
}

func (s *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	var mounts []mount.Mount
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		snap, err := storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("getting snapshot %q: %w", key, err)
		}
		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		if info.Labels[roleLabel] == imageRole {
			if !deviceExists(s.deviceName(snap.ID)) {
				id, err := devID(snap.ID)
				if err != nil {
					return err
				}
				if err := s.pool.activate(s.deviceName(snap.ID), id, s.cfg.VolumeSize, false); err != nil {
					return err
				}
			}
			mounts = []mount.Mount{{Source: devicePath(s.deviceName(snap.ID)), Type: "ext4", Options: []string{"rw"}}}
			return nil
		}
		if len(snap.ParentIDs) == 0 {
			return fmt.Errorf("snapshot %q has no image parent", key)
		}
		name := s.deviceName(snap.ID)
		if !deviceExists(name) {
			parentID, err := devID(snap.ParentIDs[0])
			if err != nil {
				return err
			}
			if err := s.pool.activate(name, parentID, s.cfg.VolumeSize, true); err != nil {
				return err
			}
		}
		if snap.Kind == snapshots.KindView {
			mounts = []mount.Mount{{Source: devicePath(name), Type: "ext4", Options: []string{"ro", "noload"}}}
			return nil
		}
		if err := s.prepareLocalLayer(snap); err != nil {
			return err
		}
		mounts = s.localMounts(snap)
		return nil
	})
	return mounts, err
}

func (s *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var info snapshots.Info
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, current, _, err := storage.GetInfo(ctx, key)
		info = current
		return err
	})
	return info, err
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	var updated snapshots.Info
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, current, _, getErr := storage.GetInfo(ctx, info.Name)
		if getErr != nil {
			return getErr
		}
		for _, field := range fieldpaths {
			if field == "labels."+roleLabel {
				return fmt.Errorf("label %q is immutable", roleLabel)
			}
		}
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[roleLabel] = current.Labels[roleLabel]
		var updateErr error
		updated, updateErr = storage.UpdateInfo(ctx, info, fieldpaths...)
		return updateErr
	})
	return updated, err
}

func (s *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var usage snapshots.Usage
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, stored, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		if info.Kind == snapshots.KindCommitted {
			usage = stored
			return nil
		}
		if info.Labels[roleLabel] != imageRole && info.Kind == snapshots.KindActive {
			du, err := fs.DiskUsage(ctx, filepath.Join(s.snapshotPath(id), "upper"))
			if err != nil {
				return err
			}
			usage = snapshots.Usage(du)
			return nil
		}
		mapped, err := mappedBytes(s.deviceName(id))
		if err != nil {
			return err
		}
		usage = snapshots.Usage{Size: mapped}
		return nil
	})
	return usage, err
}

func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

func (s *snapshotter) Cleanup(ctx context.Context) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		live, err := storage.IDMap(ctx)
		if err != nil {
			return err
		}
		devices, err := listDevices(s.cfg.PoolName + "-")
		if err != nil {
			return err
		}
		for _, device := range devices {
			id := device[len(s.cfg.PoolName)+1:]
			if _, ok := live[id]; ok {
				continue
			}
			if err := removeDevice(device); err != nil {
				return err
			}
		}
		paths, err := filepath.Glob(filepath.Join(s.cfg.Root, "snapshots", "*"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if _, ok := live[filepath.Base(path)]; ok {
				continue
			}
			if err := mount.UnmountAll(filepath.Join(path, "lower"), unix.MNT_DETACH); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *snapshotter) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		if s.ms != nil {
			closeErr = s.ms.Close()
		}
		if err := unmountLocalLowers(s.cfg.Root); err != nil && closeErr == nil {
			closeErr = err
		}
		if err := sweep(s.cfg.PoolName); err != nil && closeErr == nil {
			closeErr = err
		}
		for _, name := range []string{s.dataDevice, s.metadataDevice} {
			if name != "" {
				if strings.HasPrefix(name, "/dev/loop") {
					if out, err := exec.Command("losetup", "--detach", name).CombinedOutput(); err != nil && closeErr == nil {
						closeErr = fmt.Errorf("detaching %s: %w: %s", name, err, strings.TrimSpace(string(out)))
					}
				} else if err := removeDevice(name); err != nil && closeErr == nil {
					closeErr = err
				}
			}
		}
	})
	return closeErr
}
