// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command metalman-redfish-fixture runs a recording Redfish fixture backed by
// one libvirt domain. It is a small example wrapper around the qemusvr library
// used by the metalman smoke tests.
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
	fs.StringVar(&cfg.Domain, "domain", "", "libvirt domain name (required)")
	fs.StringVar(&cfg.MAC, "mac", "", "host NIC MAC address (required)")
	fs.StringVar(&cfg.Record, "record", "", "JSONL request record file (required)")
	fs.StringVar(&cfg.Bridge, "bridge", "", "HTTP boot boundary bridge interface")
	fs.StringVar(&cfg.DnsmasqDir, "dnsmasq-dir", "", "working directory for the HTTP boot dnsmasq")
	fs.StringVar(&cfg.Username, "username", "", "basic auth username")
	fs.StringVar(&cfg.Password, "password", "", "basic auth password")
	fs.BoolVar(&cfg.ManageBootOrder, "manage-boot-order", false, "manage libvirt boot order")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	for name, value := range map[string]string{
		"cert": cfg.Cert, "key": cfg.Key, "domain": cfg.Domain,
		"mac": cfg.MAC, "record": cfg.Record,
	} {
		if value == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}

	if (cfg.Bridge == "") != (cfg.DnsmasqDir == "") {
		return fmt.Errorf("--bridge and --dnsmasq-dir must be used together")
	}

	return qemusvr.Serve(cfg)
}
