// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"syscall"
	"testing"
)

// fakeDelRing scripts the kernel side of a pinned DEL_DEV. Each DEL_DEV
// submission takes the next entry of dels. A DEL_DEV with a nonzero
// immediate result completes without a cancel; otherwise it waits for a
// cancel, which takes the next entry of cancels.
type fakeDelRing struct {
	t *testing.T

	dels    []fakeDel
	cancels []fakeCancel

	submitErr error
	waitErr   error

	cqes []cqe

	// in-flight state
	delOut    bool
	delCur    fakeDel
	cancelOut bool

	delSubmits    int
	cancelSubmits int
	pauses        int
}

type fakeDel struct {
	// immediate completes the DEL_DEV before any cancel.
	immediate int32
	// onCancel is the DEL_DEV result once a cancel reaches it.
	onCancel int32
}

type fakeCancel struct {
	res int32
	// delFirst queues the DEL_DEV completion before the cancel's.
	delFirst bool
	// reach controls whether this cancel completes the DEL_DEV.
	reach bool
}

func (f *fakeDelRing) submitDel() error {
	if f.submitErr != nil {
		return f.submitErr
	}

	if f.delOut || f.cancelOut {
		f.t.Fatal("DEL_DEV submitted with a command still in flight")
	}

	if len(f.dels) == 0 {
		f.t.Fatal("unexpected DEL_DEV")
	}

	f.delCur, f.dels = f.dels[0], f.dels[1:]
	f.delSubmits++
	f.delOut = true

	if f.delCur.immediate != 0 {
		f.delOut = false
		f.cqes = append(f.cqes, cqe{UserData: pinnedDelUserData, Res: f.delCur.immediate})
	}

	return nil
}

func (f *fakeDelRing) submitCancel() error {
	if f.cancelOut {
		f.t.Fatal("cancel submitted with a cancel in flight")
	}

	if len(f.cancels) == 0 {
		f.t.Fatal("unexpected cancel")
	}

	c := f.cancels[0]
	f.cancels = f.cancels[1:]
	f.cancelSubmits++

	cancelCQE := cqe{UserData: pinnedCancelUserData, Res: c.res}

	var delCQE []cqe

	if c.reach && f.delOut {
		f.delOut = false
		delCQE = []cqe{{UserData: pinnedDelUserData, Res: f.delCur.onCancel}}
	}

	if c.delFirst {
		f.cqes = append(f.cqes, delCQE...)
		f.cqes = append(f.cqes, cancelCQE)
	} else {
		f.cqes = append(f.cqes, cancelCQE)
		f.cqes = append(f.cqes, delCQE...)
	}

	return nil
}

func (f *fakeDelRing) poll() (cqe, bool, error) {
	if len(f.cqes) == 0 {
		return cqe{}, false, nil
	}

	c := f.cqes[0]
	f.cqes = f.cqes[1:]

	return c, true, nil
}

func (f *fakeDelRing) wait() (cqe, error) {
	if f.waitErr != nil {
		return cqe{}, f.waitErr
	}

	if len(f.cqes) == 0 {
		f.t.Fatal("wait would block forever: no completion pending")
	}

	c, _, err := f.poll()

	return c, err
}

func (f *fakeDelRing) pause(int) { f.pauses++ }

// drained checks that nothing is left in flight or unreaped, so the
// control channel can be reused.
func (f *fakeDelRing) drained() {
	f.t.Helper()

	if f.delOut || len(f.cqes) != 0 {
		f.t.Fatalf("left in flight: del=%v cqes=%v", f.delOut, f.cqes)
	}
}

const (
	eintr     = -int32(syscall.EINTR)
	ecanceled = -int32(syscall.ECANCELED)
	ealready  = -int32(syscall.EALREADY)
	enoent    = -int32(syscall.ENOENT)
)

func TestPinnedDelInterruptedWaitIsSuccess(t *testing.T) {
	for _, delFirst := range []bool{false, true} {
		f := &fakeDelRing{
			t:       t,
			dels:    []fakeDel{{onCancel: eintr}},
			cancels: []fakeCancel{{res: ealready, reach: true, delFirst: delFirst}},
		}

		if err := pinnedDel(f); err != nil {
			t.Fatalf("delFirst=%v: %v", delFirst, err)
		}

		f.drained()

		if f.delSubmits != 1 || f.cancelSubmits != 1 {
			t.Fatalf("submits del=%d cancel=%d, want 1/1", f.delSubmits, f.cancelSubmits)
		}
	}
}

func TestPinnedDelResubmitsAfterCancelBeforeDispatch(t *testing.T) {
	f := &fakeDelRing{
		t:    t,
		dels: []fakeDel{{onCancel: ecanceled}, {onCancel: ecanceled}, {onCancel: eintr}},
		cancels: []fakeCancel{
			{res: 0, reach: true},
			{res: 0, reach: true, delFirst: true},
			{res: ealready, reach: true},
		},
	}

	if err := pinnedDel(f); err != nil {
		t.Fatal(err)
	}

	f.drained()

	if f.delSubmits != 3 {
		t.Fatalf("DEL_DEV submitted %d times, want 3", f.delSubmits)
	}
}

func TestPinnedDelRetriesCancelNotFound(t *testing.T) {
	f := &fakeDelRing{
		t:    t,
		dels: []fakeDel{{onCancel: eintr}},
		cancels: []fakeCancel{
			{res: enoent},
			{res: enoent},
			{res: ealready, reach: true},
		},
	}

	if err := pinnedDel(f); err != nil {
		t.Fatal(err)
	}

	f.drained()

	if f.cancelSubmits != 3 || f.delSubmits != 1 {
		t.Fatalf("submits del=%d cancel=%d, want 1/3", f.delSubmits, f.cancelSubmits)
	}
}

func TestPinnedDelCompletedBeforeCancelFound(t *testing.T) {
	// The cancel misses because DEL_DEV already completed; the next poll
	// reaps its result without another cancel.
	f := &fakeDelRing{
		t:       t,
		dels:    []fakeDel{{onCancel: eintr}},
		cancels: []fakeCancel{{res: enoent, reach: true, delFirst: true}},
	}

	if err := pinnedDel(f); err != nil {
		t.Fatal(err)
	}

	f.drained()
}

func TestPinnedDelImmediateResults(t *testing.T) {
	for _, tc := range []struct {
		res  int32
		want error
	}{
		{-int32(syscall.ENODEV), syscall.ENODEV},
		{-int32(syscall.EPERM), syscall.EPERM},
	} {
		f := &fakeDelRing{t: t, dels: []fakeDel{{immediate: tc.res}}}

		err := pinnedDel(f)
		if !errors.Is(err, tc.want) || errors.Is(err, errPinnedDelUnknown) {
			t.Fatalf("res %d: err = %v, want %v and known", tc.res, err, tc.want)
		}

		f.drained()

		if f.cancelSubmits != 0 {
			t.Fatalf("res %d: %d cancels, want 0", tc.res, f.cancelSubmits)
		}
	}
}

func TestPinnedDelRealErrorAfterCancel(t *testing.T) {
	f := &fakeDelRing{
		t:       t,
		dels:    []fakeDel{{onCancel: -int32(syscall.EIO)}},
		cancels: []fakeCancel{{res: ealready, reach: true}},
	}

	err := pinnedDel(f)
	if !errors.Is(err, syscall.EIO) || errors.Is(err, errPinnedDelUnknown) {
		t.Fatalf("err = %v, want EIO and known", err)
	}

	f.drained()
}

func TestPinnedDelUnknownOutcomes(t *testing.T) {
	ringErr := errors.New("io_uring_enter failed")

	for name, f := range map[string]*fakeDelRing{
		"submit": {submitErr: ringErr},
		"wait": {
			dels:    []fakeDel{{onCancel: eintr}},
			cancels: []fakeCancel{{res: ealready}},
			waitErr: ringErr,
		},
		"cancel error": {
			dels:    []fakeDel{{onCancel: eintr}},
			cancels: []fakeCancel{{res: -int32(syscall.EINVAL)}},
		},
		"never found": {
			dels:    []fakeDel{{onCancel: eintr}},
			cancels: repeatCancel(fakeCancel{res: enoent}, pinnedDelCancels),
		},
	} {
		f.t = t

		err := pinnedDel(f)
		if !errors.Is(err, errPinnedDelUnknown) {
			t.Fatalf("%s: err = %v, want errPinnedDelUnknown", name, err)
		}
	}
}

func TestPinnedDelBoundedResubmits(t *testing.T) {
	dels := make([]fakeDel, pinnedDelSubmits)
	for i := range dels {
		dels[i] = fakeDel{onCancel: ecanceled}
	}

	f := &fakeDelRing{
		t:       t,
		dels:    dels,
		cancels: repeatCancel(fakeCancel{res: 0, reach: true}, pinnedDelSubmits),
	}

	err := pinnedDel(f)
	if err == nil || errors.Is(err, errPinnedDelUnknown) {
		t.Fatalf("err = %v, want a known failure", err)
	}

	f.drained()

	if f.delSubmits != pinnedDelSubmits {
		t.Fatalf("DEL_DEV submitted %d times, want %d", f.delSubmits, pinnedDelSubmits)
	}
}

func repeatCancel(c fakeCancel, n int) []fakeCancel {
	s := make([]fakeCancel, n)
	for i := range s {
		s[i] = c
	}

	return s
}
