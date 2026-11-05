// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// devInfo returns GET_DEV_INFO for id over a fresh control channel.
func devInfo(t *testing.T, id int) (ctrlDevInfo, error) {
	t.Helper()

	c, err := openCtrl()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = c.close() }()

	var info ctrlDevInfo

	res, err := c.doPayload(uCmdGetDevInfo, uint32(id), 0, nil, structBytes(&info))
	if err == nil {
		err = resErr("GET_DEV_INFO", res)
	}

	return info, err
}

// TestIntegrationStartRejectsReusedID deletes the device Start added
// after ADD_DEV but before Start opens its char device, and adds a
// replacement under the same id with cookie 0, which Start never uses.
// Start must then pin the replacement, see the wrong cookie, and leave it
// alone: not configured, started, stopped or deleted.
func TestIntegrationStartRejectsReusedID(t *testing.T) {
	requireFaultVM(t)

	cfg, err := Config{Queues: 1, Depth: 4, Size: 1 << 20}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	replaced := -1

	testHooks.openChar = func(path string) (int, error) {
		testHooks.openChar = nil

		id, err := strconv.Atoi(strings.TrimPrefix(path, "/dev/ublkc"))
		if err != nil {
			return -1, err
		}

		if err := Delete(id); err != nil {
			return -1, err
		}

		c, err := openCtrl()
		if err != nil {
			return -1, err
		}

		got, err := c.addDev(id, cfg, 0)
		if err := errors.Join(err, c.close()); err != nil {
			return -1, err
		}

		replaced = got

		return openWhenPresent(path)
	}

	t.Cleanup(func() {
		testHooks.openChar = nil

		if replaced >= 0 {
			if err := Delete(replaced); err != nil {
				t.Errorf("delete replacement %d: %v", replaced, err)
			}
		}
	})

	d, err := Start(context.Background(), Config{ID: -1, Size: 1 << 20, Queues: 1, Depth: 4}, newPatternHandler(1<<20))
	if err == nil {
		_ = d.Close()

		t.Fatal("Start accepted a device that reused its id")
	}

	if !errors.Is(err, errUnpinnedLeft) {
		t.Fatalf("Start = %v, want errUnpinnedLeft", err)
	}

	if replaced < 0 {
		t.Fatalf("replacement not created: %v", err)
	}

	info, qerr := devInfo(t, replaced)
	if qerr != nil {
		t.Fatalf("replacement %d gone after Start: %v", replaced, qerr)
	}

	if info.UblksrvFlags != 0 {
		t.Fatalf("replacement cookie = %#x, want 0", info.UblksrvFlags)
	}

	if info.State == ublkSDevLive {
		t.Fatal("replacement was started")
	}
}
