// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const controlPath = "/dev/ublk-control"

// erestartsys is the kernel-internal ERESTARTSYS, which can leak into CQE
// results of interruptible waits such as START_DEV.
const erestartsys = 512

// ublkFCmdIoctlEncode is UBLK_F_CMD_IOCTL_ENCODE.
const ublkFCmdIoctlEncode = 1 << 6

// ctrl sends control commands over /dev/ublk-control.
type ctrl struct {
	mu   sync.Mutex
	fd   int
	ring *uring
}

func openCtrl() (*ctrl, error) {
	fd, err := unix.Open(controlPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ublk: open %s: %w", controlPath, err)
	}

	ring, err := newURing(4)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ublk: control ring: %w", err), unix.Close(fd))
	}

	return &ctrl{fd: fd, ring: ring}, nil
}

func (c *ctrl) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error

	if c.ring != nil {
		errs = append(errs, c.ring.close())
		c.ring = nil
	}

	if c.fd >= 0 {
		errs = append(errs, unix.Close(c.fd))
		c.fd = -1
	}

	return errors.Join(errs...)
}

// do sends one control command and returns the kernel result. buf, if
// non-nil, is passed as the command's addr/len payload.
func (c *ctrl) do(op, devID uint32, data uint64, buf unsafe.Pointer, bufLen int) (int32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ring == nil {
		return 0, fmt.Errorf("ublk: control channel closed")
	}

	s := c.ring.getSQE()
	if s == nil {
		return 0, errRingFull
	}

	s.Opcode = ioringOpURingCmd
	s.Fd = int32(c.fd)
	s.Off = uint64(op)
	s.UserData = uint64(op)

	cmd := (*ctrlCmd)(unsafe.Pointer(&cmdArea(s)[0]))
	cmd.DevID = devID
	cmd.QueueID = 0xffff
	cmd.Data = data

	if buf != nil {
		cmd.Addr = uint64(uintptr(buf))
		cmd.Len = uint16(bufLen)
	}

	c.ring.flushSQ()

	cq, err := c.ring.waitCQE()

	runtime.KeepAlive(buf)

	if err != nil {
		return 0, err
	}

	return cq.Res, nil
}

func resErr(name string, res int32) error {
	if res >= 0 {
		return nil
	}

	return fmt.Errorf("ublk: %s: %w", name, syscall.Errno(-res))
}

func (c *ctrl) addDev(id int, cfg Config) (int, error) {
	info := ctrlDevInfo{
		NrHwQueues:    uint16(cfg.Queues),
		QueueDepth:    uint16(cfg.Depth),
		MaxIOBufBytes: uint32(cfg.MaxIOBytes),
		DevID:         uint32(int32(id)), //nolint:gosec // -1 intentionally maps to U32_MAX
		Flags:         ublkFCmdIoctlEncode,
	}

	res, err := c.do(uCmdAddDev, info.DevID, 0, unsafe.Pointer(&info), int(unsafe.Sizeof(info)))
	if err != nil {
		return 0, err
	}

	if err := resErr("ADD_DEV", res); err != nil {
		return 0, err
	}

	return int(info.DevID), nil
}

func (c *ctrl) setParams(id int, cfg Config) error {
	p := buildParams(cfg)

	res, err := c.do(uCmdSetParams, uint32(id), 0, unsafe.Pointer(&p), int(unsafe.Sizeof(p)))
	if err != nil {
		return err
	}

	return resErr("SET_PARAMS", res)
}

func (c *ctrl) startDev(id int) error {
	for range 16 {
		res, err := c.do(uCmdStartDev, uint32(id), uint64(os.Getpid()), nil, 0)
		if err != nil {
			return err
		}

		if res == -int32(syscall.EINTR) || res == -erestartsys {
			continue
		}

		return resErr("START_DEV", res)
	}

	return fmt.Errorf("ublk: START_DEV: interrupted too many times")
}

func (c *ctrl) stopDev(id int) error {
	res, err := c.do(uCmdStopDev, uint32(id), 0, nil, 0)
	if err != nil {
		return err
	}

	return resErr("STOP_DEV", res)
}

func (c *ctrl) delDev(id int) error {
	res, err := c.do(uCmdDelDev, uint32(id), 0, nil, 0)
	if err != nil {
		return err
	}

	return resErr("DEL_DEV", res)
}
