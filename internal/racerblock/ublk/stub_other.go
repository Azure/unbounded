// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package ublk

import "context"

// Device is a running ublk device. It is unavailable on this platform.
type Device struct{}

// Start returns ErrUnsupported on this platform.
func Start(_ context.Context, _ Config, _ Handler) (*Device, error) { return nil, ErrUnsupported }

// Delete returns ErrUnsupported on this platform.
func Delete(_ int) error { return ErrUnsupported }

// ID returns -1.
func (d *Device) ID() int { return -1 }

// Path returns "".
func (d *Device) Path() string { return "" }

// Done returns a closed channel.
func (d *Device) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}

// Err returns ErrUnsupported.
func (d *Device) Err() error { return ErrUnsupported }

// Close returns nil.
func (d *Device) Close() error { return nil }
