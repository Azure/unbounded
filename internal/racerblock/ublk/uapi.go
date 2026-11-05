// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

// Mirrors of <linux/ublk_cmd.h>. Sizes and offsets are checked in tests.

// ublk control and io command numbers.
const (
	cmdGetDevInfo = 0x02
	cmdAddDev     = 0x04
	cmdDelDev     = 0x05
	cmdStartDev   = 0x06
	cmdStopDev    = 0x07
	cmdSetParams  = 0x08

	ioFetchReq         = 0x20
	ioCommitAndFetchRq = 0x21
)

// Flags, attributes and special values.
const (
	ublkAttrReadOnly   = 1 << 0
	ublkParamTypeBasic = 1 << 0

	ublksrvCmdBufOffset = 0
)

// Linux generic ioctl encoding (asm-generic/ioctl.h).
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uint32) uint32 {
	return dir<<iocDirShift | typ<<iocTypeShift | nr<<iocNRShift | size<<iocSizeShift
}

func iowr(nr, size uint32) uint32 { return ioc(iocRead|iocWrite, 'u', nr, size) }

func ior(nr, size uint32) uint32 { return ioc(iocRead, 'u', nr, size) }

// Ioctl-encoded ublk commands (UBLK_U_CMD_* and UBLK_U_IO_*).
var (
	uCmdGetDevInfo       = ior(cmdGetDevInfo, ctrlCmdSize)
	uCmdAddDev           = iowr(cmdAddDev, ctrlCmdSize)
	uCmdDelDev           = iowr(cmdDelDev, ctrlCmdSize)
	uCmdStartDev         = iowr(cmdStartDev, ctrlCmdSize)
	uCmdStopDev          = iowr(cmdStopDev, ctrlCmdSize)
	uCmdSetParams        = iowr(cmdSetParams, ctrlCmdSize)
	uIOFetchReq          = iowr(ioFetchReq, ioCmdSize)
	uIOCommitAndFetchReq = iowr(ioCommitAndFetchRq, ioCmdSize)
)

const (
	ctrlCmdSize = 32
	ioCmdSize   = 16
	ioDescSize  = 24
)

// ctrlCmd is struct ublksrv_ctrl_cmd.
type ctrlCmd struct {
	DevID      uint32
	QueueID    uint16
	Len        uint16
	Addr       uint64
	Data       uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

// ctrlDevInfo is struct ublksrv_ctrl_dev_info.
type ctrlDevInfo struct {
	NrHwQueues    uint16
	QueueDepth    uint16
	State         uint16
	Pad0          uint16
	MaxIOBufBytes uint32
	DevID         uint32
	UblksrvPid    int32
	Pad1          uint32
	Flags         uint64
	UblksrvFlags  uint64
	OwnerUID      uint32
	OwnerGID      uint32
	Reserved1     uint64
	Reserved2     uint64
}

// ioDesc is struct ublksrv_io_desc.
type ioDesc struct {
	OpFlags     uint32
	NrSectors   uint32
	StartSector uint64
	Addr        uint64
}

// ioCmd is struct ublksrv_io_cmd.
type ioCmd struct {
	QID    uint16
	Tag    uint16
	Result int32
	Addr   uint64
}

// paramBasic is struct ublk_param_basic.
type paramBasic struct {
	Attrs            uint32
	LogicalBSShift   uint8
	PhysicalBSShift  uint8
	IOOptShift       uint8
	IOMinShift       uint8
	MaxSectors       uint32
	ChunkSectors     uint32
	DevSectors       uint64
	VirtBoundaryMask uint64
}

// paramDiscard is struct ublk_param_discard.
type paramDiscard struct {
	DiscardAlignment      uint32
	DiscardGranularity    uint32
	MaxDiscardSectors     uint32
	MaxWriteZeroesSectors uint32
	MaxDiscardSegments    uint16
	Reserved0             uint16
}

// paramDevt is struct ublk_param_devt.
type paramDevt struct {
	CharMajor uint32
	CharMinor uint32
	DiskMajor uint32
	DiskMinor uint32
}

// paramZoned is struct ublk_param_zoned.
type paramZoned struct {
	MaxOpenZones         uint32
	MaxActiveZones       uint32
	MaxZoneAppendSectors uint32
	Reserved             [20]uint8
}

// params is struct ublk_params.
type params struct {
	Len     uint32
	Types   uint32
	Basic   paramBasic
	Discard paramDiscard
	Devt    paramDevt
	Zoned   paramZoned
}

// buildParams returns the SET_PARAMS payload for cfg.
func buildParams(cfg Config) params {
	var p params

	p.Len = uint32(sizeofParams)
	p.Types = ublkParamTypeBasic
	p.Basic = paramBasic{
		Attrs:           ublkAttrReadOnly,
		LogicalBSShift:  9,
		PhysicalBSShift: 12,
		IOOptShift:      12,
		IOMinShift:      12,
		MaxSectors:      uint32(cfg.MaxIOBytes >> 9),
		DevSectors:      uint64(cfg.Size >> 9),
	}

	return p
}

const sizeofParams = 112

// descArraySize is the per-queue io descriptor mmap length.
func descArraySize(depth, pageSize int) int {
	n := depth * ioDescSize

	return (n + pageSize - 1) / pageSize * pageSize
}

// descArrayOffset is the mmap offset of queue q's descriptor array. The
// kernel strides queues by the size for UBLK_MAX_QUEUE_DEPTH, not by the
// configured depth (see ublk_ch_mmap and ublk_max_cmd_buf_size).
func descArrayOffset(q, pageSize int) int64 {
	return int64(ublksrvCmdBufOffset + q*descArraySize(MaxQueueDepth, pageSize))
}
