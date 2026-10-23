// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"os"
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

// ctrlPayloadSize is the size of the control payload page. It must hold the
// largest payload, which is ublkParams.
const ctrlPayloadSize = 4096

// ctrl sends control commands over /dev/ublk-control.
//
// Command payloads live in payload, an anonymous mmap that the Go runtime
// never moves or frees. The kernel gets its address as a plain integer, so
// Go heap or stack memory is not safe there: stacks can move, and the GC
// does not see the integer as a reference.
type ctrl struct {
	mu      sync.Mutex
	fd      int
	ring    *uring
	payload []byte
	// poisoned is set when a command was sent but its completion was not
	// seen. The kernel may still use payload, so it is never unmapped.
	poisoned bool
}

func openCtrl() (*ctrl, error) {
	fd, err := unix.Open(controlPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ublk: open %s: %w", controlPath, err)
	}

	return newCtrl(fd)
}

// newCtrl sets up the ring and payload for an open control fd. It takes
// ownership of fd and closes it on error.
func newCtrl(fd int) (*ctrl, error) {
	payload, err := unix.Mmap(-1, 0, ctrlPayloadSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ublk: control payload: %w", err), unix.Close(fd))
	}

	ring, err := newURing(4)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ublk: control ring: %w", err), unix.Munmap(payload), unix.Close(fd))
	}

	return &ctrl{fd: fd, ring: ring, payload: payload}, nil
}

// isPoisoned reports whether a command's completion was missed.
func (c *ctrl) isPoisoned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.poisoned
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

	// If a command may still be in flight, keep the page mapped. Closing
	// the ring does not wait for in-flight requests, and the kernel would
	// write through the old address into whatever is mapped there next.
	// This leaks one page, and only after a failed io_uring_enter.
	if c.payload != nil && !c.poisoned {
		errs = append(errs, unix.Munmap(c.payload))
	}

	c.payload = nil

	return errors.Join(errs...)
}

// do sends one control command with no payload and returns the kernel
// result. buf must be nil and bufLen must be 0. Use doPayload for commands
// with a payload. The signature is kept for existing callers.
func (c *ctrl) do(op, devID uint32, data uint64, buf unsafe.Pointer, bufLen int) (int32, error) {
	if buf != nil || bufLen != 0 {
		return 0, fmt.Errorf("ublk: control payload must use doPayload")
	}

	return c.doPayload(op, devID, data, nil, nil)
}

// doPayload sends one control command and returns the kernel result. in, if
// non-empty, is copied into the payload page, which is passed as the
// command's addr/len. After completion, the first len(out) bytes of the page
// are copied to out. The payload size is max(len(in), len(out)).
func (c *ctrl) doPayload(op, devID uint32, data uint64, in, out []byte) (int32, error) {
	if testHooks.ctrlSent != nil {
		testHooks.ctrlSent(op, devID)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ring == nil {
		return 0, fmt.Errorf("ublk: control channel closed")
	}

	if c.poisoned {
		return 0, fmt.Errorf("ublk: control channel unusable after a failed command")
	}

	n := max(len(in), len(out))
	if n > len(c.payload) {
		return 0, fmt.Errorf("ublk: control payload too large: %d", n)
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

	if n > 0 {
		clear(c.payload[:n])
		copy(c.payload, in)
		cmd.Addr = uint64(uintptr(unsafe.Pointer(&c.payload[0])))
		cmd.Len = uint16(n)
	}

	c.ring.flushSQ()

	cq, err := c.ring.waitCQE()
	if err != nil {
		// The SQE is published and may have been taken by the kernel, so
		// its completion is unknown. Stop using the ring and the payload.
		c.poisoned = true
		return 0, err
	}

	copy(out, c.payload[:n])

	return cq.Res, nil
}

// structBytes returns the bytes of *v. v must not contain Go pointers.
func structBytes[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
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

	b := structBytes(&info)

	res, err := c.doPayload(uCmdAddDev, info.DevID, 0, b, b)
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

	res, err := c.doPayload(uCmdSetParams, uint32(id), 0, structBytes(&p), nil)
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
