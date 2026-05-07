// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// unping sends health check probes to a remote unbounded-net node and prints
// round-trip times in a format similar to standard ping.
package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
	"github.com/Azure/unbounded/internal/version"

	flag "github.com/spf13/pflag"
)

func main() {
	var (
		count       int
		interval    time.Duration
		timeoutSec  float64
		timeoutMs   int
		sourceAddr  string
		port        int
		srcHostname string
		dstHostname string
		showVersion bool
	)

	flag.IntVarP(&count, "count", "c", 0, "Number of probes to send (0 = until stopped)")
	flag.DurationVarP(&interval, "interval", "i", time.Second, "Interval between probes")
	flag.Float64VarP(&timeoutSec, "timeout", "w", 5, "Timeout in seconds for each probe")
	flag.IntVarP(&timeoutMs, "timeout-ms", "W", 0, "Timeout in milliseconds (overrides -w)")
	flag.StringVarP(&sourceAddr, "interface", "I", "", "Source IP address or interface name")
	flag.IntVarP(&port, "port", "p", 9997, "UDP port for health check probes")
	flag.StringVar(&srcHostname, "src-hostname", "", "Source hostname in probes (default: OS hostname)")
	flag.StringVar(&dstHostname, "dst-hostname", "", "Destination hostname in probes (default: target address)")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: unping [options] <target-ip>\n\n")
		fmt.Fprintf(os.Stderr, "Send unbounded-net health check probes to a remote node.\n\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if showVersion {
		fmt.Printf("unping %s (commit %s, built %s)\n", version.Version, version.GitCommit, version.BuildTime)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		os.Exit(1)
	}

	target := args[0]

	timeout := time.Duration(timeoutSec * float64(time.Second))
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	targetIP := net.ParseIP(target)
	if targetIP == nil {
		ips, err := net.LookupIP(target)
		if err != nil || len(ips) == 0 {
			fmt.Fprintf(os.Stderr, "unping: cannot resolve %s: %v\n", target, err)
			os.Exit(1)
		}

		targetIP = ips[0]
	}

	if srcHostname == "" {
		srcHostname, _ = os.Hostname() //nolint:errcheck
		if srcHostname == "" {
			srcHostname = "unping"
		}
	}

	if dstHostname == "" {
		dstHostname = target
	}

	// Bind local UDP socket
	var localAddr *net.UDPAddr

	if sourceAddr != "" {
		srcIP := net.ParseIP(sourceAddr)
		if srcIP == nil {
			// Try as interface name
			iface, err := net.InterfaceByName(sourceAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "unping: invalid source %s: %v\n", sourceAddr, err)
				os.Exit(1)
			}

			addrs, err := iface.Addrs()
			if err != nil || len(addrs) == 0 {
				fmt.Fprintf(os.Stderr, "unping: no addresses on interface %s\n", sourceAddr)
				os.Exit(1)
			}

			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					srcIP = ipNet.IP
					break
				}
			}

			if srcIP == nil {
				// Fall back to first address
				if ipNet, ok := addrs[0].(*net.IPNet); ok {
					srcIP = ipNet.IP
				}
			}
		}

		if srcIP != nil {
			localAddr = &net.UDPAddr{IP: srcIP}
		}
	}

	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unping: failed to bind UDP socket: %v\n", err)
		os.Exit(1)
	}

	defer func() { _ = conn.Close() }() //nolint:errcheck

	localPort := conn.LocalAddr().(*net.UDPAddr).Port //nolint:errcheck
	remoteAddr := &net.UDPAddr{IP: targetIP, Port: port}

	fmt.Printf("UNPING %s (%s) port %d, source port %d\n", dstHostname, targetIP, port, localPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Stats
	var (
		sent, received           int
		minRTT, maxRTT, totalRTT time.Duration
		rttSamples               []float64
	)

	minRTT = time.Duration(math.MaxInt64)

	// Reply receiver goroutine
	replyCh := make(chan replyResult, 16)
	go receiveReplies(ctx, conn, replyCh)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Send first probe immediately
	sendAndPrint := func(seq uint64) {
		sent++

		sendProbe(conn, remoteAddr, srcHostname, dstHostname, seq)

		select {
		case reply := <-replyCh:
			if reply.err != nil {
				fmt.Printf("From %s: seq=%d error: %v\n", targetIP, seq, reply.err)
			} else {
				received++

				rtt := reply.rtt
				if rtt < minRTT {
					minRTT = rtt
				}

				if rtt > maxRTT {
					maxRTT = rtt
				}

				totalRTT += rtt
				rttSamples = append(rttSamples, float64(rtt.Microseconds()))
				fmt.Printf("Reply from %s: seq=%d time=%.3f ms\n",
					targetIP, seq, float64(rtt.Microseconds())/1000.0)
			}
		case <-time.After(timeout):
			fmt.Printf("Request timeout for seq %d\n", seq)
		case <-ctx.Done():
			return
		}
	}

	var seq uint64

	seq++
	sendAndPrint(seq)

	for {
		select {
		case <-sigCh:
			cancel()
			printStats(dstHostname, targetIP.String(), sent, received, minRTT, maxRTT, totalRTT, rttSamples)
			os.Exit(0)
		case <-ticker.C:
			seq++
			sendAndPrint(seq)

			if count > 0 && int(seq) >= count {
				printStats(dstHostname, targetIP.String(), sent, received, minRTT, maxRTT, totalRTT, rttSamples)
				os.Exit(0)
			}
		case <-ctx.Done():
			return
		}
	}
}

type replyResult struct {
	rtt time.Duration
	seq uint64
	err error
}

func sendProbe(conn *net.UDPConn, addr *net.UDPAddr, srcHost, dstHost string, seq uint64) {
	pkt := &pb.HealthCheckPacket{
		SourceHostname:      srcHost,
		DestinationHostname: dstHost,
		SequenceNumber:      seq,
		TimestampNs:         time.Now().UnixNano(),
		Type:                pb.PacketType_REQUEST,
	}

	data, err := proto.Marshal(pkt)
	if err != nil {
		return
	}

	_, _ = conn.WriteToUDP(data, addr) //nolint:errcheck
}

func receiveReplies(ctx context.Context, conn *net.UDPConn, ch chan<- replyResult) {
	buf := make([]byte, 1500)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) //nolint:errcheck

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}

			continue
		}

		now := time.Now()

		pkt := &pb.HealthCheckPacket{}
		if err := proto.Unmarshal(buf[:n], pkt); err != nil {
			continue
		}

		if pkt.Type != pb.PacketType_REPLY {
			continue
		}

		rtt := time.Duration(now.UnixNano()-pkt.TimestampNs) * time.Nanosecond
		ch <- replyResult{rtt: rtt, seq: pkt.SequenceNumber}
	}
}

func printStats(host, ip string, sent, received int, minRTT, maxRTT, totalRTT time.Duration, samples []float64) {
	fmt.Printf("\n--- %s (%s) unping statistics ---\n", host, ip)

	loss := 0.0
	if sent > 0 {
		loss = float64(sent-received) / float64(sent) * 100
	}

	fmt.Printf("%d probes transmitted, %d received, %.1f%% loss\n", sent, received, loss)

	if received > 0 {
		avg := totalRTT / time.Duration(received)
		// Calculate stddev
		avgUs := float64(avg.Microseconds())

		var sumSqDiff float64

		for _, s := range samples {
			diff := s - avgUs
			sumSqDiff += diff * diff
		}

		stddev := math.Sqrt(sumSqDiff / float64(len(samples)))
		fmt.Printf("rtt min/avg/max/stddev = %.3f/%.3f/%.3f/%.3f ms\n",
			float64(minRTT.Microseconds())/1000.0,
			float64(avg.Microseconds())/1000.0,
			float64(maxRTT.Microseconds())/1000.0,
			stddev/1000.0)
	}
}
