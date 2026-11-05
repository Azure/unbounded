// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// hung is set when a test gave up on a hung device. Hook cleanups skip
// resetting testHooks once it is set.
var hung atomic.Bool

// abortHung ends the test binary when a kernel fault test hangs.
//
// A hung Start or teardown leaves queue goroutines running that read
// testHooks, so the test cannot fail normally: t.Fatal would run the
// cleanup that resets the hooks while those goroutines still use them,
// and the rest of the suite would run beside a device that never
// stopped. These tests run only in a disposable VM, so abortHung marks
// the hooks as still in use, prints every goroutine and the kernel
// stack of every blocked task, then panics. The testing package runs
// cleanups on the way out and then ends the whole suite.
func abortHung(t *testing.T, msg string) {
	t.Helper()

	hung.Store(true)

	var b strings.Builder

	fmt.Fprintf(&b, "%s: %s\n", t.Name(), msg)
	b.WriteString(hangDump())
	panic(b.String())
}

// hangDump returns all goroutine stacks and the kernel stacks of this
// process's threads and of every task in uninterruptible sleep.
func hangDump() string {
	var b strings.Builder

	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	b.WriteString("=== goroutines\n")
	b.Write(buf[:n])

	self := fmt.Sprintf("/proc/%d/", os.Getpid())

	tasks, _ := filepath.Glob("/proc/[0-9]*/task/[0-9]*")
	for _, task := range tasks {
		stat, err := os.ReadFile(task + "/stat")
		if err != nil {
			continue
		}

		// The state follows the parenthesized command name.
		i := strings.LastIndexByte(string(stat), ')')
		if i < 0 || i+2 >= len(stat) {
			continue
		}

		state := stat[i+2]
		if state != 'D' && !strings.HasPrefix(task, self) {
			continue
		}

		comm, _ := os.ReadFile(task + "/comm")
		stack, err := os.ReadFile(task + "/stack")

		fmt.Fprintf(&b, "=== kernel task %s comm=%s state=%c\n", task, strings.TrimSpace(string(comm)), state)

		if err != nil {
			fmt.Fprintf(&b, "stack unavailable: %v\n", err)
			continue
		}

		b.Write(stack)
	}

	return b.String()
}
