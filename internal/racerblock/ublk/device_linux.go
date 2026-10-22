// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// uCmdDelDevAsync is UBLK_U_CMD_DEL_DEV_ASYNC (Linux 6.9+). Unlike
// DEL_DEV it does not wait for the char device to be released.
var uCmdDelDevAsync = ior(0x14, uint32(unsafe.Sizeof(ctrlCmd{})))

// nodeWaitTimeout bounds how long Start waits for device nodes to appear.
const nodeWaitTimeout = 10 * time.Second

// Device is a running read-only ublk block device.
type Device struct {
	cfg     Config
	handler Handler
	id      int

	ctrl   *ctrl
	charFd int
	queues []*queue

	hctx    context.Context //nolint:containedctx // handler context lives as long as the device
	hcancel context.CancelFunc
	workers sync.WaitGroup

	started bool

	// startMu guards starting and startErr. While starting is set,
	// fail only records the first error: Start owns teardown until it
	// returns, so queue resources cannot be released while queues are
	// still being launched or START_DEV is in flight.
	startMu  sync.Mutex
	starting bool
	startErr error

	// stopAsync runs teardown off the caller's goroutine. Tests replace
	// it to observe failures without a kernel device.
	stopAsync func(error)
	// launch starts a queue thread. Tests replace it to inject errors.
	launch func(q *queue, ready chan<- error)

	closing atomic.Bool // set when teardown begins

	once     sync.Once
	done     chan struct{}
	err      error // cause of stop, valid after done is closed
	closeErr error // teardown error, valid after done is closed
}

// Start creates the device, starts serving it with h and returns once
// /dev/ublkb<ID> exists. The device is torn down when ctx is canceled.
func Start(ctx context.Context, cfg Config, h Handler) (*Device, error) {
	if h == nil {
		return nil, errors.New("ublk: nil handler")
	}

	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c, err := openCtrl()
	if err != nil {
		return nil, err
	}

	id, err := c.addDev(cfg.ID, cfg)
	if err != nil {
		return nil, errors.Join(err, c.close())
	}

	d := &Device{
		cfg:     cfg,
		handler: h,
		id:      id,
		ctrl:    c,
		charFd:  -1,
		done:    make(chan struct{}),
	}
	d.init()
	d.hctx, d.hcancel = context.WithCancel(ctx)

	if err := d.runStartup(d.start); err != nil {
		d.teardown(err)
		return nil, err
	}

	go d.watch(ctx)

	cfg.Logger.Info("ublk device started", "id", id, "path", d.Path(), "size", cfg.Size,
		"queues", cfg.Queues, "depth", cfg.Depth, "maxIOBytes", cfg.MaxIOBytes)

	return d, nil
}

// init sets the default hooks and enters the startup phase.
func (d *Device) init() {
	d.stopAsync = func(err error) { go d.teardown(err) }
	d.launch = func(q *queue, ready chan<- error) { go q.run(ready) }
	d.starting = true
}

// runStartup runs start while fail defers teardown to the caller, then
// leaves the startup phase. A queue error recorded during startup is
// returned even when start itself succeeded, so the caller tears down
// exactly once and only after every queue has been launched.
func (d *Device) runStartup(start func() error) error {
	err := start()

	d.startMu.Lock()
	d.starting = false
	recorded := d.startErr
	d.startMu.Unlock()

	if err == nil {
		err = recorded
	}

	return err
}

// startupErr returns the first queue error recorded during startup.
func (d *Device) startupErr() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()

	return d.startErr
}

func (d *Device) start() error {
	if err := d.ctrl.setParams(d.id, d.cfg); err != nil {
		return err
	}

	charPath := "/dev/ublkc" + strconv.Itoa(d.id)

	fd, err := openWhenPresent(charPath)
	if err != nil {
		return err
	}

	d.charFd = fd

	for i := range d.cfg.Queues {
		q, err := newQueue(d, i)
		if err != nil {
			return err
		}

		d.queues = append(d.queues, q)
	}

	// Every queue must have all of its FETCH_REQs submitted before
	// START_DEV, which waits for them.
	ready := make(chan error, len(d.queues))
	for _, q := range d.queues {
		q.running = true
		d.launch(q, ready)
	}

	var errs []error

	for range d.queues {
		if err := <-ready; err != nil {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("ublk: submit FETCH_REQ: %w", err)
	}

	// A queue may have failed after its FETCH_REQs were submitted; do
	// not start a device that already has a dead queue.
	if err := d.startupErr(); err != nil {
		return err
	}

	d.started = true

	if err := d.ctrl.startDev(d.id); err != nil {
		return err
	}

	return waitForNode(d.Path())
}

func openWhenPresent(path string) (int, error) {
	deadline := time.Now().Add(nodeWaitTimeout)

	for {
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err == nil {
			return fd, nil
		}

		if !errors.Is(err, unix.ENOENT) || time.Now().After(deadline) {
			return -1, fmt.Errorf("ublk: open %s: %w", path, err)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func waitForNode(path string) error {
	deadline := time.Now().Add(nodeWaitTimeout)

	for {
		_, err := os.Stat(path)
		if err == nil {
			return nil
		}

		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return fmt.Errorf("ublk: wait for %s: %w", path, err)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func (d *Device) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		d.teardown(ctx.Err())
	case <-d.done:
	}
}

// fail tears the device down asynchronously because of err. During
// startup it only records err; Start returns it and tears down after all
// queues are launched, which avoids releasing a queue that is about to
// run. fail never blocks, so a failing queue thread cannot deadlock
// against Start waiting on START_DEV.
func (d *Device) fail(err error) {
	d.startMu.Lock()
	if d.starting {
		if d.startErr == nil {
			d.startErr = err
		}

		d.startMu.Unlock()

		return
	}
	d.startMu.Unlock()

	d.stopAsync(err)
}

// teardown stops the device once and records cause. It blocks until the
// device is fully released.
func (d *Device) teardown(cause error) {
	d.once.Do(func() {
		var errs []error

		d.closing.Store(true)
		d.hcancel()

		// After an external STOP_DEV or DEL_DEV the kernel may report
		// ENODEV for our own control commands; that is not a teardown
		// failure.
		external := errors.Is(cause, errStoppedExternally)
		keep := func(err error) {
			if err != nil && (!external || !errors.Is(err, syscall.ENODEV)) {
				errs = append(errs, err)
			}
		}

		if d.started {
			keep(d.ctrl.stopDev(d.id))
		}

		for _, q := range d.queues {
			q.stop()
		}

		for _, q := range d.queues {
			// Queues that never ran have no exited signal to wait on.
			if q.running {
				<-q.exited
			}
		}

		d.workers.Wait()

		for _, q := range d.queues {
			errs = append(errs, q.release())
		}

		if !d.started {
			// Never live; STOP_DEV makes DEL_DEV safe. It may fail
			// harmlessly when the device was never started.
			if err := d.ctrl.stopDev(d.id); err != nil {
				d.cfg.Logger.Debug("ublk: STOP_DEV before start", "id", d.id, "err", err)
			}
		}

		// The kernel keeps the id allocated while the char device is
		// open, so deleting before closing it can never hit another
		// device that reused the id. Kernels without DEL_DEV_ASYNC fall
		// back to DEL_DEV after the close, since DEL_DEV waits for it.
		deleted := false

		if d.charFd >= 0 {
			res, err := d.ctrl.do(uCmdDelDevAsync, uint32(d.id), 0, nil, 0)
			if err == nil {
				err = resErr("DEL_DEV_ASYNC", res)
			}

			switch {
			case err == nil:
				deleted = true
			case errors.Is(err, syscall.ENODEV):
				// Already deleted by someone else.
				deleted = true
			default:
				d.cfg.Logger.Debug("ublk: DEL_DEV_ASYNC failed, falling back to DEL_DEV", "id", d.id, "err", err)
			}

			errs = append(errs, unix.Close(d.charFd))
			d.charFd = -1
		}

		if !deleted {
			keep(d.ctrl.delDev(d.id))
		}

		errs = append(errs, d.ctrl.close())

		d.err = cause
		d.closeErr = errors.Join(errs...)

		if cause != nil && d.cfg.Logger != nil {
			d.cfg.Logger.Info("ublk device stopped", "id", d.id, "cause", cause)
		}

		close(d.done)
	})
	<-d.done
}

// ID returns the kernel device id.
func (d *Device) ID() int { return d.id }

// Path returns the block device path, "/dev/ublkb<ID>".
func (d *Device) Path() string { return "/dev/ublkb" + strconv.Itoa(d.id) }

// Done is closed when the device has stopped for any reason, including
// another process stopping or deleting it.
func (d *Device) Done() <-chan struct{} { return d.done }

// Err reports why the device stopped. It is nil while running and after a
// clean Close.
func (d *Device) Err() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}

// Close stops the device, drains queues and in-flight handlers and deletes
// it. It is idempotent and safe to call concurrently.
func (d *Device) Close() error {
	d.teardown(nil)
	return d.closeErr
}

// Delete makes a best-effort attempt to stop and delete device id, for
// example one left behind by a crashed process. It rejects an id outside
// [0, MaxDeviceID] before opening the control device.
func Delete(id int) error {
	if err := validateDeviceID(id); err != nil {
		return err
	}

	c, err := openCtrl()
	if err != nil {
		return err
	}

	stopErr := c.stopDev(id)
	if err := c.delDev(id); err != nil {
		return errors.Join(stopErr, err, c.close())
	}

	return c.close()
}
