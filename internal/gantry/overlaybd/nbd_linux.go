// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nbdRequestMagic = uint32(0x25609513)
	nbdReplyMagic   = uint32(0x67446698)
	nbdRequestSize  = 28
	nbdReplySize    = 16

	nbdCommandRead       = uint32(0)
	nbdCommandWrite      = uint32(1)
	nbdCommandDisconnect = uint32(2)
	nbdCommandFlush      = uint32(3)
	nbdCommandTrim       = uint32(4)
	nbdCommandMask       = uint32(0xffff)

	nbdSetSocket    = uintptr(0xab00)
	nbdSetBlockSize = uintptr(0xab01)
	nbdDoIt         = uintptr(0xab03)
	nbdClearSocket  = uintptr(0xab04)
	nbdClearQueue   = uintptr(0xab05)
	nbdSetSizeBlock = uintptr(0xab07)
	nbdDisconnect   = uintptr(0xab08)
	nbdSetTimeout   = uintptr(0xab09)
	nbdSetFlags     = uintptr(0xab0a)

	nbdFlagHasFlags = 1
	nbdFlagReadOnly = 2
	nbdTimeout      = 180

	maxNBDRequestSize = 32 << 20
)

type sizedReaderAt interface {
	io.ReaderAt
	Size() int64
}

// NBDDevice exposes a read-only virtual disk through a Linux NBD device.
type NBDDevice struct {
	path       string
	device     *os.File
	server     *os.File
	done       chan struct{}
	serveError error
	doItError  error
	closeOnce  sync.Once
}

// AttachNBD binds reader to an unused /dev/nbdN device until Close is called.
func AttachNBD(path string, reader sizedReaderAt) (*NBDDevice, error) {
	if reader == nil || reader.Size() <= 0 || reader.Size()%sectorSize != 0 {
		return nil, fmt.Errorf("overlaybd NBD: invalid virtual size")
	}

	device, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("overlaybd NBD: open %s: %w", path, err)
	}

	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		_ = device.Close() //nolint:errcheck // socketpair error is the actionable failure
		return nil, fmt.Errorf("overlaybd NBD: socketpair: %w", err)
	}

	server := os.NewFile(uintptr(sockets[0]), "overlaybd-nbd-server")
	kernelSocket := os.NewFile(uintptr(sockets[1]), "overlaybd-nbd-kernel")
	cleanup := func() {
		_ = server.Close()       //nolint:errcheck // cleanup after an earlier setup failure
		_ = kernelSocket.Close() //nolint:errcheck // cleanup after an earlier setup failure
		_ = device.Close()       //nolint:errcheck // cleanup after an earlier setup failure
	}

	if err := ioctlSetInt(device.Fd(), nbdSetSocket, sockets[1]); err != nil {
		cleanup()
		return nil, fmt.Errorf("overlaybd NBD: set socket: %w", err)
	}

	if err := ioctlSetInt(device.Fd(), nbdSetBlockSize, int(sectorSize)); err != nil {
		_ = ioctl(device.Fd(), nbdClearSocket) //nolint:errcheck // rollback after an earlier setup failure

		cleanup()

		return nil, fmt.Errorf("overlaybd NBD: set block size: %w", err)
	}

	if err := ioctlSetInt(device.Fd(), nbdSetSizeBlock, int(reader.Size()/sectorSize)); err != nil {
		_ = ioctl(device.Fd(), nbdClearSocket) //nolint:errcheck // rollback after an earlier setup failure

		cleanup()

		return nil, fmt.Errorf("overlaybd NBD: set size: %w", err)
	}

	if err := ioctlSetInt(device.Fd(), nbdSetTimeout, nbdTimeout); err != nil {
		_ = ioctl(device.Fd(), nbdClearSocket) //nolint:errcheck // rollback after an earlier setup failure

		cleanup()

		return nil, fmt.Errorf("overlaybd NBD: set timeout: %w", err)
	}

	if err := ioctlSetInt(device.Fd(), nbdSetFlags, nbdFlagHasFlags|nbdFlagReadOnly); err != nil {
		_ = ioctl(device.Fd(), nbdClearSocket) //nolint:errcheck // rollback after an earlier setup failure

		cleanup()

		return nil, fmt.Errorf("overlaybd NBD: set flags: %w", err)
	}

	if err := kernelSocket.Close(); err != nil {
		_ = ioctl(device.Fd(), nbdClearSocket) //nolint:errcheck // rollback after an earlier setup failure

		cleanup()

		return nil, fmt.Errorf("overlaybd NBD: close kernel socket: %w", err)
	}

	nbd := &NBDDevice{path: path, device: device, server: server, done: make(chan struct{})}
	go nbd.run(reader)

	if err := waitNBDReady(nbd); err != nil {
		_ = nbd.Close() //nolint:errcheck // activation error is the actionable failure
		return nil, err
	}

	return nbd, nil
}

func (d *NBDDevice) Path() string { return d.path }

func (d *NBDDevice) run(reader sizedReaderAt) {
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveNBD(d.server, reader) }()

	d.doItError = ioctl(d.device.Fd(), nbdDoIt)
	_ = d.server.Close() //nolint:errcheck // wakes the protocol loop after NBD_DO_IT exits
	d.serveError = <-serveDone
	_ = ioctl(d.device.Fd(), nbdClearQueue)  //nolint:errcheck // best-effort kernel teardown
	_ = ioctl(d.device.Fd(), nbdClearSocket) //nolint:errcheck // best-effort kernel teardown
	close(d.done)
}

func waitNBDReady(device *NBDDevice) error {
	pidPath := filepath.Join("/sys/class/block", filepath.Base(device.path), "pid")

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-device.done:
			return fmt.Errorf("overlaybd NBD: %s stopped during activation: %w", device.path, device.doItError)
		case <-timer.C:
			return fmt.Errorf("overlaybd NBD: %s activation timed out", device.path)
		case <-ticker.C:
			pid, err := os.ReadFile(pidPath)
			if err == nil && valueIsPositive(strings.TrimSpace(string(pid))) {
				return nil
			}
		}
	}
}

func valueIsPositive(value string) bool {
	return value != "" && value != "0"
}

func (d *NBDDevice) Close() error {
	d.closeOnce.Do(func() {
		_ = ioctl(d.device.Fd(), nbdDisconnect) //nolint:errcheck // either path may already be disconnected
		_ = d.server.Close()                    //nolint:errcheck // wakes the protocol loop if still active
		<-d.done
		_ = d.device.Close() //nolint:errcheck // loop errors below are more actionable
	})

	if d.doItError != nil && !errors.Is(d.doItError, unix.EPIPE) {
		return fmt.Errorf("overlaybd NBD: device loop: %w", d.doItError)
	}

	if d.serveError != nil && !errors.Is(d.serveError, os.ErrClosed) && !errors.Is(d.serveError, io.EOF) {
		return fmt.Errorf("overlaybd NBD: serve: %w", d.serveError)
	}

	return nil
}

func serveNBD(connection io.ReadWriter, reader sizedReaderAt) error {
	header := make([]byte, nbdRequestSize)
	for {
		if _, err := io.ReadFull(connection, header); err != nil {
			return err
		}

		if binary.BigEndian.Uint32(header[:4]) != nbdRequestMagic {
			return errors.New("overlaybd NBD: invalid request magic")
		}

		command := binary.BigEndian.Uint32(header[4:8]) & nbdCommandMask
		handle := header[8:16]
		offset := binary.BigEndian.Uint64(header[16:24])

		length := binary.BigEndian.Uint32(header[24:28])
		if length > maxNBDRequestSize {
			return fmt.Errorf("overlaybd NBD: request length %d exceeds limit", length)
		}

		switch command {
		case nbdCommandRead:
			if err := serveNBDRead(connection, reader, handle, offset, length); err != nil {
				return err
			}
		case nbdCommandWrite:
			if _, err := io.CopyN(io.Discard, connection, int64(length)); err != nil {
				return fmt.Errorf("overlaybd NBD: discard write: %w", err)
			}

			if err := writeNBDReply(connection, handle, unix.EPERM, nil); err != nil {
				return err
			}
		case nbdCommandFlush:
			if err := writeNBDReply(connection, handle, 0, nil); err != nil {
				return err
			}
		case nbdCommandTrim:
			if err := writeNBDReply(connection, handle, unix.EPERM, nil); err != nil {
				return err
			}
		case nbdCommandDisconnect:
			return nil
		default:
			if err := writeNBDReply(connection, handle, unix.EINVAL, nil); err != nil {
				return err
			}
		}
	}
}

func serveNBDRead(connection io.Writer, reader sizedReaderAt, handle []byte, offset uint64, length uint32) error {
	if offset > uint64(reader.Size()) || uint64(length) > uint64(reader.Size())-offset {
		return writeNBDReply(connection, handle, unix.EINVAL, nil)
	}

	data := make([]byte, length)
	if _, err := reader.ReadAt(data, int64(offset)); err != nil {
		return writeNBDReply(connection, handle, unix.EIO, nil)
	}

	return writeNBDReply(connection, handle, 0, data)
}

func writeNBDReply(writer io.Writer, handle []byte, errno unix.Errno, data []byte) error {
	reply := make([]byte, nbdReplySize)
	binary.BigEndian.PutUint32(reply[:4], nbdReplyMagic)
	binary.BigEndian.PutUint32(reply[4:8], uint32(errno))
	copy(reply[8:16], handle)

	if err := writeFull(writer, reply); err != nil {
		return fmt.Errorf("overlaybd NBD: write reply: %w", err)
	}

	if len(data) != 0 {
		if err := writeFull(writer, data); err != nil {
			return fmt.Errorf("overlaybd NBD: write data: %w", err)
		}
	}

	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}

		if n == 0 {
			return io.ErrShortWrite
		}

		data = data[n:]
	}

	return nil
}

func ioctl(fd, request uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, 0)
	if errno != 0 {
		return errno
	}

	return nil
}

func ioctlSetInt(fd, request uintptr, value int) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, uintptr(value))
	if errno != 0 {
		return errno
	}

	return nil
}
