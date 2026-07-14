// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command metalman-redfish-fixture runs a recording Redfish fixture backed by
// one QEMU/KVM virtual machine launched directly (no libvirt). It is a small
// example wrapper around the qemusvr library used by the metalman smoke tests.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Azure/unbounded/internal/metalman/redfish/qemusvr"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("metalman-redfish-fixture: %v", err)
	}
}

func run() error {
	var cfg qemusvr.Config

	fs := flag.NewFlagSet("metalman-redfish-fixture", flag.ExitOnError)
	fs.StringVar(&cfg.Bind, "bind", "127.0.0.1", "address to bind")
	fs.IntVar(&cfg.Port, "port", 8443, "port to listen on")
	fs.StringVar(&cfg.Cert, "cert", "", "TLS certificate file (required)")
	fs.StringVar(&cfg.Key, "key", "", "TLS key file (required)")
	fs.StringVar(&cfg.Domain, "domain", "", "guest name and Redfish system Id (required)")
	fs.StringVar(&cfg.MAC, "mac", "", "guest NIC MAC address (required)")
	fs.StringVar(&cfg.Record, "record", "", "JSONL request record file (required)")
	fs.StringVar(&cfg.Disk, "disk", "", "guest qcow2 disk image path (required)")
	fs.IntVar(&cfg.MemoryMiB, "memory-mib", 4096, "guest RAM in MiB")
	fs.IntVar(&cfg.VCPUs, "vcpus", 2, "guest vCPU count")
	fs.StringVar(&cfg.OVMFCode, "ovmf-code", "", "read-only OVMF firmware code pflash image (required)")
	fs.StringVar(&cfg.OVMFVars, "ovmf-vars", "", "OVMF variables template copied to the NVRAM store (required)")
	fs.BoolVar(&cfg.SecureBoot, "secure-boot", false, "enable SMM and secure boot pflash")
	fs.StringVar(&cfg.StateDir, "state-dir", "", "working directory for NVRAM, sockets, and TPM state (required)")
	fs.StringVar(&cfg.Bridge, "bridge", "", "boundary bridge interface the fixture creates and manages")
	fs.StringVar(&cfg.BridgeAddress, "bridge-address", "", "host IP assigned to the bridge (the guest gateway)")
	fs.IntVar(&cfg.BridgePrefix, "bridge-prefix", 24, "CIDR prefix length for the bridge address")
	fs.StringVar(&cfg.DnsmasqDir, "dnsmasq-dir", "", "working directory for the HTTP boot dnsmasq")
	fs.StringVar(&cfg.Username, "username", "", "basic auth username")
	fs.StringVar(&cfg.Password, "password", "", "basic auth password")
	fs.BoolVar(&cfg.ManageBootOrder, "manage-boot-order", false, "manage guest boot order for PXE overrides")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	for name, value := range map[string]string{
		"cert": cfg.Cert, "key": cfg.Key, "domain": cfg.Domain,
		"mac": cfg.MAC, "record": cfg.Record, "disk": cfg.Disk,
		"ovmf-code": cfg.OVMFCode, "ovmf-vars": cfg.OVMFVars, "state-dir": cfg.StateDir,
	} {
		if value == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}

	if cfg.Bridge != "" && cfg.BridgeAddress == "" {
		return fmt.Errorf("--bridge-address is required when --bridge is set")
	}

	if cfg.DnsmasqDir != "" && cfg.Bridge == "" {
		return fmt.Errorf("--dnsmasq-dir requires --bridge")
	}

	return qemusvr.Serve(cfg)
}
