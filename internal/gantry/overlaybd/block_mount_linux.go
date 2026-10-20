// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/distribution/reference"
)

// NBDMounter mounts stacked LSMT images through a pool of Linux NBD devices.
type NBDMounter struct {
	endpoint   string
	client     *http.Client
	deviceGlob string

	mu   sync.Mutex
	used map[string]struct{}
}

func NewNBDMounter(endpoint string, client *http.Client, deviceGlob string) (*NBDMounter, error) {
	if _, err := parseGantryEndpoint(endpoint); err != nil {
		return nil, err
	}

	if deviceGlob == "" {
		deviceGlob = "/dev/nbd*"
	}

	_, err := filepath.Glob(deviceGlob)
	if err != nil {
		return nil, fmt.Errorf("overlaybd block mounter: device glob: %w", err)
	}

	if client == nil {
		client = http.DefaultClient
	}

	return &NBDMounter{endpoint: endpoint, client: client, deviceGlob: deviceGlob, used: make(map[string]struct{})}, nil
}

func (m *NBDMounter) Mount(ctx context.Context, id string, remoteLayers []RemoteLayer, mountpoint string) (io.Closer, error) {
	if len(remoteLayers) == 0 {
		return nil, errors.New("overlaybd block mounter: no layers")
	}

	layers := make([]*LSMTLayer, 0, len(remoteLayers))
	filesystem := "ext4"

	var mountOptions []string

	for _, remoteLayer := range remoteLayers {
		origin, err := blobOriginURL(remoteLayer.ImageRef, remoteLayer.Digest.String())
		if err != nil {
			return nil, err
		}

		reader, err := NewRangeReader(m.client, m.endpoint, origin, remoteLayer.Size)
		if err != nil {
			return nil, err
		}

		layer, err := OpenLSMTLayer(reader, remoteLayer.Size)
		if err != nil {
			return nil, fmt.Errorf("overlaybd block mounter: open %s: %w", remoteLayer.Digest, err)
		}

		layers = append(layers, layer)

		if remoteLayer.FSType != "" {
			filesystem, mountOptions, err = parseFilesystemSpec(remoteLayer.FSType)
			if err != nil {
				return nil, err
			}
		}
	}

	image, err := NewLSMTImage(layers...)
	if err != nil {
		return nil, err
	}

	if isEROFS(image) {
		filesystem = "erofs"
		mountOptions = nil
	}

	device, err := m.attach(image)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		_ = device.Close() //nolint:errcheck // directory error is the actionable failure
		m.release(device.Path())

		return nil, err
	}

	if err := mount.UnmountAll(mountpoint, 0); err != nil {
		_ = device.Close() //nolint:errcheck // stale unmount error is the actionable failure
		m.release(device.Path())

		return nil, fmt.Errorf("overlaybd block mounter: clear stale mount %s: %w", id, err)
	}

	mountOptions = append(mountOptions, "ro")
	if err := mount.All([]mount.Mount{{Source: device.Path(), Type: filesystem, Options: mountOptions}}, mountpoint); err != nil {
		_ = device.Close() //nolint:errcheck // mount error is the actionable failure
		m.release(device.Path())

		return nil, fmt.Errorf("overlaybd block mounter: mount %s: %w", id, err)
	}

	return &mountedNBD{owner: m, device: device, mountpoint: mountpoint}, nil
}

func (m *NBDMounter) attach(image *LSMTImage) (*NBDDevice, error) {
	devices, err := filepath.Glob(m.deviceGlob)
	if err != nil {
		return nil, fmt.Errorf("overlaybd block mounter: device glob: %w", err)
	}

	sort.Slice(devices, func(i, j int) bool { return nbdDeviceNumber(devices[i]) < nbdDeviceNumber(devices[j]) })

	if len(devices) == 0 {
		return nil, fmt.Errorf("overlaybd block mounter: no devices match %s", m.deviceGlob)
	}

	var errs []error

	for _, path := range devices {
		m.mu.Lock()
		if _, used := m.used[path]; used {
			m.mu.Unlock()
			continue
		}

		m.used[path] = struct{}{}
		m.mu.Unlock()

		device, err := AttachNBD(path, image)
		if err == nil {
			return device, nil
		}

		m.release(path)

		errs = append(errs, err)
	}

	return nil, fmt.Errorf("overlaybd block mounter: no available NBD device: %w", errors.Join(errs...))
}

func (m *NBDMounter) release(path string) {
	m.mu.Lock()
	delete(m.used, path)
	m.mu.Unlock()
}

type mountedNBD struct {
	owner      *NBDMounter
	device     *NBDDevice
	mountpoint string
	mu         sync.Mutex
	closed     bool
	err        error
}

func (m *mountedNBD) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return m.err
	}

	if err := mount.UnmountAll(m.mountpoint, 0); err != nil {
		return err
	}

	m.err = m.device.Close()
	m.owner.release(m.device.Path())
	m.closed = true

	return m.err
}

func blobOriginURL(imageRef, blobDigest string) (string, error) {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", fmt.Errorf("overlaybd block mounter: parse image reference %q: %w", imageRef, err)
	}

	host := reference.Domain(named)

	repository := reference.Path(named)
	if host == "" || repository == "" {
		return "", fmt.Errorf("overlaybd block mounter: incomplete image reference %q", imageRef)
	}

	origin := url.URL{Scheme: "https", Host: host, Path: "/v2/" + repository + "/blobs/" + blobDigest}

	return origin.String(), nil
}

func parseFilesystemSpec(spec string) (string, []string, error) {
	parts := strings.Split(spec, ";")

	filesystem := parts[0]
	if filesystem != "ext4" && filesystem != "erofs" {
		return "", nil, fmt.Errorf("overlaybd block mounter: unsupported filesystem %q", filesystem)
	}

	var options []string
	if len(parts) > 1 && parts[1] != "" {
		options = strings.Split(parts[1], ",")
	}

	return filesystem, options, nil
}

func isEROFS(image *LSMTImage) bool {
	var magic [4]byte
	if _, err := image.ReadAt(magic[:], 1024); err != nil {
		return false
	}

	return binary.LittleEndian.Uint32(magic[:]) == 0xe0f5e1e2
}

func nbdDeviceNumber(path string) int {
	number, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "nbd"))
	if err != nil {
		return int(^uint(0) >> 1)
	}

	return number
}
