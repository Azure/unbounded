// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

import (
	"testing"
	"unsafe"
)

// Expected values were produced by compiling a throwaway C program against
// /usr/include/linux/ublk_cmd.h (kernel 7.0 headers).
func TestUAPISizes(t *testing.T) {
	cases := []struct {
		name      string
		got, want uintptr
	}{
		{"ublksrv_ctrl_cmd", unsafe.Sizeof(ctrlCmd{}), 32},
		{"ublksrv_ctrl_cmd.addr", unsafe.Offsetof(ctrlCmd{}.Addr), 8},
		{"ublksrv_ctrl_cmd.data", unsafe.Offsetof(ctrlCmd{}.Data), 16},
		{"ublksrv_ctrl_dev_info", unsafe.Sizeof(ctrlDevInfo{}), 64},
		{"ublksrv_ctrl_dev_info.max_io_buf_bytes", unsafe.Offsetof(ctrlDevInfo{}.MaxIOBufBytes), 8},
		{"ublksrv_ctrl_dev_info.dev_id", unsafe.Offsetof(ctrlDevInfo{}.DevID), 12},
		{"ublksrv_ctrl_dev_info.ublksrv_pid", unsafe.Offsetof(ctrlDevInfo{}.UblksrvPid), 16},
		{"ublksrv_ctrl_dev_info.flags", unsafe.Offsetof(ctrlDevInfo{}.Flags), 24},
		{"ublksrv_ctrl_dev_info.owner_uid", unsafe.Offsetof(ctrlDevInfo{}.OwnerUID), 40},
		{"ublksrv_io_desc", unsafe.Sizeof(ioDesc{}), 24},
		{"ublksrv_io_desc.start_sector", unsafe.Offsetof(ioDesc{}.StartSector), 8},
		{"ublksrv_io_desc.addr", unsafe.Offsetof(ioDesc{}.Addr), 16},
		{"ublksrv_io_cmd", unsafe.Sizeof(ioCmd{}), 16},
		{"ublksrv_io_cmd.result", unsafe.Offsetof(ioCmd{}.Result), 4},
		{"ublksrv_io_cmd.addr", unsafe.Offsetof(ioCmd{}.Addr), 8},
		{"ublk_param_basic", unsafe.Sizeof(paramBasic{}), 32},
		{"ublk_param_basic.max_sectors", unsafe.Offsetof(paramBasic{}.MaxSectors), 8},
		{"ublk_param_basic.dev_sectors", unsafe.Offsetof(paramBasic{}.DevSectors), 16},
		{"ublk_param_basic.virt_boundary_mask", unsafe.Offsetof(paramBasic{}.VirtBoundaryMask), 24},
		{"ublk_param_discard", unsafe.Sizeof(paramDiscard{}), 20},
		{"ublk_param_devt", unsafe.Sizeof(paramDevt{}), 16},
		{"ublk_param_zoned", unsafe.Sizeof(paramZoned{}), 32},
		{"ublk_params", unsafe.Sizeof(params{}), sizeofParams},
		{"ublk_params.basic", unsafe.Offsetof(params{}.Basic), 8},
		{"ctrlCmdSize", ctrlCmdSize, unsafe.Sizeof(ctrlCmd{})},
		{"ioCmdSize", ioCmdSize, unsafe.Sizeof(ioCmd{})},
		{"ioDescSize", ioDescSize, unsafe.Sizeof(ioDesc{})},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestIoctlEncoding(t *testing.T) {
	cases := []struct {
		name      string
		got, want uint32
	}{
		{"UBLK_U_CMD_ADD_DEV", uCmdAddDev, 3223352580},
		{"UBLK_U_CMD_DEL_DEV", uCmdDelDev, 3223352581},
		{"UBLK_U_CMD_START_DEV", uCmdStartDev, 3223352582},
		{"UBLK_U_CMD_STOP_DEV", uCmdStopDev, 3223352583},
		{"UBLK_U_CMD_SET_PARAMS", uCmdSetParams, 3223352584},
		{"UBLK_U_CMD_GET_DEV_INFO", uCmdGetDevInfo, 2149610754},
		{"UBLK_U_IO_FETCH_REQ", uIOFetchReq, 3222304032},
		{"UBLK_U_IO_COMMIT_AND_FETCH_REQ", uIOCommitAndFetchReq, 3222304033},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

func TestBuildParams(t *testing.T) {
	cfg, err := Config{ID: -1, Size: 64<<20 + 512, MaxIOBytes: 512 << 10}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	p := buildParams(cfg)
	if p.Len != sizeofParams || p.Types != ublkParamTypeBasic {
		t.Fatalf("header: %+v", p)
	}

	b := p.Basic
	if b.Attrs != ublkAttrReadOnly || b.LogicalBSShift != 9 || b.PhysicalBSShift != 12 ||
		b.IOMinShift != 12 || b.IOOptShift != 12 {
		t.Fatalf("basic: %+v", b)
	}

	if b.MaxSectors != 1024 || b.DevSectors != (64<<20+512)>>9 {
		t.Fatalf("sectors: %+v", b)
	}

	if p.Discard != (paramDiscard{}) {
		t.Fatalf("discard must not be advertised: %+v", p.Discard)
	}
}

func TestDescArraySize(t *testing.T) {
	cases := []struct{ depth, page, want int }{
		{1, 4096, 4096},
		{128, 4096, 4096},
		{171, 4096, 8192}, // 171*24 = 4104
		{4096, 4096, 24 * 4096},
		{128, 65536, 65536},
	}
	for _, c := range cases {
		if got := descArraySize(c.depth, c.page); got != c.want {
			t.Errorf("descArraySize(%d, %d) = %d, want %d", c.depth, c.page, got, c.want)
		}
	}
}

func TestDescArrayOffset(t *testing.T) {
	if got := descArrayOffset(0, 4096); got != 0 {
		t.Errorf("q0 = %d", got)
	}

	// 4096 * 24 bytes = 96 KiB per queue regardless of configured depth.
	if got := descArrayOffset(3, 4096); got != 3*96*1024 {
		t.Errorf("q3 = %d", got)
	}

	if got := descArrayOffset(1, 65536); got != 2*65536 {
		t.Errorf("q1 64k pages = %d", got)
	}
}
