// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
)

// findTraceMap locates the unb_trace ringbuf created by the
// unbounded_encap eBPF program. The map is discovered via MapGetNextID
// the same way findMap does for unb_endpts.
func findTraceMap() (*ebpf.Map, error) {
	id := ebpf.MapID(0)

	for {
		var err error

		id, err = ebpf.MapGetNextID(id)
		if err != nil {
			break
		}

		m, err := ebpf.NewMapFromID(id)
		if err != nil {
			continue
		}

		info, err := m.Info()
		if err != nil {
			_ = m.Close() //nolint:errcheck
			continue
		}

		if info.Name == ebpfpkg.TraceMapName {
			return m, nil
		}

		_ = m.Close() //nolint:errcheck
	}

	return nil, fmt.Errorf("BPF map %q not found (is the unbounded_encap program loaded?)", ebpfpkg.TraceMapName)
}

// streamTrace opens the trace ringbuf, prints decoded events to stdout
// until SIGINT or SIGTERM, then closes the reader. The BPF producer
// detects the absence of a consumer automatically: bpf_ringbuf_reserve
// returns NULL once the ring is full, so events emitted while no one
// is reading simply pile up until the ring saturates.
//
// To avoid showing the operator a flood of stale events that accumulated
// before they attached, we record CLOCK_MONOTONIC at startup (the same
// clock bpf_ktime_get_ns reads from) and drop any event whose timestamp
// predates it.
func streamTrace(opts textOptions) error {
	m, err := findTraceMap()
	if err != nil {
		return err
	}
	defer m.Close() //nolint:errcheck

	reader, err := ringbuf.NewReader(m)
	if err != nil {
		return fmt.Errorf("open ringbuf reader: %w", err)
	}

	var startTs unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &startTs); err != nil {
		return fmt.Errorf("read CLOCK_MONOTONIC: %w", err)
	}

	startKtime := uint64(startTs.Sec)*1_000_000_000 + uint64(startTs.Nsec) //nolint:gosec

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig

		_ = reader.Close() //nolint:errcheck
	}()

	fmt.Fprintln(os.Stderr, "unroute: tracing unbounded_encap (Ctrl-C to stop)")

	var dropped uint64

	for {
		rec, err := reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				if dropped > 0 {
					fmt.Fprintf(os.Stderr, "unroute: dropped %d pre-attach event(s)\n", dropped)
				}

				return nil
			}

			return fmt.Errorf("read ringbuf: %w", err)
		}

		ev, decodeErr := decodeTraceEvent(rec.RawSample)
		if decodeErr != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", decodeErr)
			continue
		}

		if ev.TsNs < startKtime {
			dropped++
			continue
		}

		printTraceEvent(ev, opts)
	}
}

func decodeTraceEvent(buf []byte) (ebpfpkg.RawTraceEvent, error) {
	var ev ebpfpkg.RawTraceEvent

	expected := binary.Size(ev)

	if len(buf) < expected {
		return ev, fmt.Errorf("short trace event: %d bytes, want %d", len(buf), expected)
	}

	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &ev); err != nil {
		return ev, fmt.Errorf("decode trace event: %w", err)
	}

	return ev, nil
}

func printTraceEvent(ev ebpfpkg.RawTraceEvent, opts textOptions) {
	ts := time.Unix(0, int64(ev.TsNs)).UTC().Format("15:04:05.000000")

	proto, src, dst := formatFlowParts(ev)

	lpm := "miss"
	nh := ""
	via := ""
	dev := ""
	encapProto := ""
	vni := ""

	if ev.LpmPrefixlen != 0 || ev.NhCount != 0 {
		chosen := "-"
		if ev.ChosenIdx >= 0 {
			// chosen_idx is 0-based in BPF; render 1-based so "nh=1/4"
			// reads naturally ("first of four") instead of "zeroth of four".
			chosen = fmt.Sprintf("%d", ev.ChosenIdx+1)
		}

		lpm = fmt.Sprintf("/%d", ev.LpmPrefixlen)
		nh = fmt.Sprintf("%s/%d", chosen, ev.NhCount)
		via = formatTraceAddr(ev.Remote[:])
		dev = fmt.Sprintf("if%d", ev.Ifindex)
		encapProto = protocolName(ev.Protocol)
		vni = fmt.Sprintf("vni=%d", ev.Vni)
	}

	keyPart := "-"
	if ev.NeedsKey != 0 {
		keyPart = formatBPFRet(ev.SetKeyRet, opts)
	}

	// Column widths chosen so a typical IPv4 trace line stays on a single
	// terminal screen while leaving room for the long fields (encap +
	// verdict). IPv6 lines may overflow individual columns; that is
	// preferable to producing wildly uneven layout in the common case.
	fmt.Printf("%s cpu=%-2d %-4s %-21s -> %-21s len=%-5d lpm=%-5s nh=%-4s via=%-15s dev=%-4s %-9s %-5s set_key=%-3s redir=%-3s %s\n",
		ts, ev.Cpu, proto, src, dst, ev.SkbLen,
		lpm, nh, via, dev, encapProto, vni,
		keyPart, formatBPFRet(ev.RedirectRet, opts), verdictString(ev.Verdict))
}

// formatFlowParts returns the L4 protocol name and the source/destination
// host:port pair (or bare addresses for non-port protocols / non-IP frames).
// Splitting the components lets the caller pad each column independently.
func formatFlowParts(ev ebpfpkg.RawTraceEvent) (proto, src, dst string) {
	if ev.EthProto != 0x0800 && ev.EthProto != 0x86dd {
		return ethProtoName(ev.EthProto), "-", "-"
	}

	saddr := formatTraceAddr(ev.Saddr[:])
	daddr := formatTraceAddr(ev.Daddr[:])

	proto = ipProtoName(ev.IpProto)

	if hasPorts(ev.IpProto) {
		return proto, joinHostPort(saddr, ev.Sport), joinHostPort(daddr, ev.Dport)
	}

	return proto, saddr, daddr
}

func hasPorts(ipProto uint8) bool {
	switch ipProto {
	case 6, 17, 132: // TCP, UDP, SCTP
		return true
	}

	return false
}

func ipProtoName(p uint8) string {
	switch p {
	case 0:
		return "?"
	case 1:
		return "ICMP"
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 58:
		return "ICMPv6"
	case 132:
		return "SCTP"
	default:
		return fmt.Sprintf("ipproto=%d", p)
	}
}

func joinHostPort(addr string, port uint16) string {
	if isIPv6Literal(addr) {
		return fmt.Sprintf("[%s]:%d", addr, port)
	}

	return fmt.Sprintf("%s:%d", addr, port)
}

func isIPv6Literal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}

	return false
}

func formatTraceAddr(addr16 []byte) string {
	if len(addr16) != 16 {
		return "?"
	}
	// IPv4-mapped: ::ffff:<v4>
	zero := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if bytes.Equal(addr16[:10], zero) && addr16[10] == 0xff && addr16[11] == 0xff {
		return net.IPv4(addr16[12], addr16[13], addr16[14], addr16[15]).String()
	}

	if bytes.Equal(addr16, make([]byte, 16)) {
		return "::"
	}

	return net.IP(addr16).String()
}

func ethProtoName(p uint16) string {
	switch p {
	case 0x0800:
		return "IPv4"
	case 0x86dd:
		return "IPv6"
	case 0:
		return "(none)"
	default:
		return fmt.Sprintf("eth=0x%04x", p)
	}
}

func verdictString(v int32) string {
	switch v {
	case 0:
		return "TC_ACT_OK"
	case 2:
		return "TC_ACT_SHOT"
	case 7:
		return "TC_ACT_REDIRECT"
	default:
		if v < 0 {
			return fmt.Sprintf("err(%d)", v)
		}

		return fmt.Sprintf("%d", v)
	}
}

// formatBPFRet renders a BPF helper return value with red highlighting
// (when color is enabled) for non-zero negative results, since those
// indicate kernel-side failures the operator likely wants to see.
func formatBPFRet(ret int32, opts textOptions) string {
	s := fmt.Sprintf("%d", ret)
	if ret < 0 && opts.useColor {
		return ansiRed + s + ansiReset
	}

	return s
}
