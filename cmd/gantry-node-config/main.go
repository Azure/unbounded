// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Azure/unbounded/internal/gantry/containerdconfig"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gantry-node-config", flag.ContinueOnError)
	action := fs.String("action", "apply", "configuration action: apply or restore")
	configPath := fs.String("config", "/host/etc/containerd/config.toml", "host containerd config")
	dropInPath := fs.String("drop-in", "/host/etc/containerd/conf.d/99-gantry-overlaybd.toml", "managed containerd drop-in")
	stateDir := fs.String("state", "/host/var/lib/gantry/snapshotter-config", "configuration state directory")

	address := fs.String("address", "/run/gantry-overlaybd/gantry-overlaybd.sock", "host snapshotter socket")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %v", fs.Args())
	}

	manager := containerdconfig.Manager{
		ConfigPath:      *configPath,
		DropInPath:      *dropInPath,
		StateDir:        *stateDir,
		Address:         *address,
		SnapshotterName: "gantry-overlaybd",
	}

	var (
		result containerdconfig.Result
		err    error
	)

	switch *action {
	case "apply":
		result, err = manager.Apply()
	case "restore":
		result, err = manager.Restore()
	default:
		return fmt.Errorf("unknown action %q", *action)
	}

	if err != nil {
		return err
	}

	if result.Changed {
		fmt.Println("changed")
	} else {
		fmt.Println("unchanged")
	}

	return nil
}
