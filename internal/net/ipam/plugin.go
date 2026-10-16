// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/allocator"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"

	ipamproto "github.com/Azure/unbounded/internal/net/ipam/proto"
)

var networkNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Plugin uses the host-local store. DataDir is injectable for isolated tests;
// the installed binary always uses host-local's default directory.
type Plugin struct {
	DataDir     string
	printResult func(types.Result, string) error
}

func (p Plugin) Functions() skel.CNIFuncs {
	return skel.CNIFuncs{
		Add: p.Add, Del: p.Del, Check: p.Check,
		GC:     func(*skel.CmdArgs) error { return errors.New("unbounded-ipam does not support GC") },
		Status: func(*skel.CmdArgs) error { return errors.New("unbounded-ipam does not support STATUS") },
	}
}

func parseInput(args *skel.CmdArgs) (map[string]json.RawMessage, string, string, error) {
	if args.ContainerID == "" || args.IfName == "" {
		return nil, "", "", errors.New("CNI container ID and interface name are required")
	}

	var input map[string]json.RawMessage
	if err := json.Unmarshal(args.StdinData, &input); err != nil {
		return nil, "", "", fmt.Errorf("parse CNI input: %w", err)
	}

	var name string
	if err := json.Unmarshal(input["name"], &name); err != nil || !networkNamePattern.MatchString(name) {
		return nil, "", "", errors.New("invalid CNI network name")
	}

	var options struct {
		Type       string `json:"type"`
		SocketPath string `json:"socketPath"`
	}
	if err := json.Unmarshal(input["ipam"], &options); err != nil || options.Type != "unbounded-ipam" {
		return nil, "", "", errors.New("ipam.type must be unbounded-ipam")
	}

	return input, name, cmp.Or(options.SocketPath, DefaultSocketPath), nil
}

func (p Plugin) Add(args *skel.CmdArgs) (returnErr error) {
	input, _, path, err := parseInput(args)
	if err != nil {
		return err
	}

	var callerIPAM map[string]json.RawMessage
	if err := json.Unmarshal(input["ipam"], &callerIPAM); err != nil {
		return err
	}

	for field := range callerIPAM {
		if field != "type" && field != "socketPath" {
			return fmt.Errorf("unsupported caller IPAM option %q; allocation configuration must come from the agent", field)
		}
	}

	for field, raw := range input {
		if !strings.EqualFold(field, "runtimeConfig") {
			continue
		}

		var runtimeConfig map[string]json.RawMessage
		if err := json.Unmarshal(raw, &runtimeConfig); err != nil {
			return err
		}

		for option := range runtimeConfig {
			if strings.EqualFold(option, "ipRanges") {
				return errors.New("runtimeConfig.ipRanges cannot override verified Node PodCIDRs")
			}
		}
	}

	config, err := GetConfig(context.Background(), path)
	if err != nil {
		return err
	}

	hostLocal, err := hostLocalConfig(config, p.DataDir)
	if err != nil {
		return err
	}

	input["ipam"] = hostLocal

	data, err := json.Marshal(input)
	if err != nil {
		return err
	}

	conf, confVersion, err := allocator.LoadIPAMConfig(data, args.Args)
	if err != nil {
		return err
	}

	store, err := disk.New(conf.Name, conf.DataDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()

	requested := make(map[string]net.IP)
	for _, ip := range conf.IPArgs {
		requested[ip.String()] = ip
	}

	result := &current.Result{CNIVersion: current.ImplementedSpecVersion}

	var allocated []*allocator.IPAllocator

	defer func() {
		if returnErr != nil {
			for _, alloc := range allocated {
				returnErr = errors.Join(returnErr, alloc.Release(args.ContainerID, args.IfName))
			}
		}
	}()

	for index, ranges := range conf.Ranges {
		alloc := allocator.NewIPAllocator(&ranges, store, index)

		var requestedIP net.IP

		for key, ip := range requested {
			if ranges.Contains(ip) {
				requestedIP = ip

				delete(requested, key)

				break
			}
		}

		address, err := alloc.Get(args.ContainerID, args.IfName, requestedIP)
		if err != nil {
			return fmt.Errorf("allocate range %d: %w", index, err)
		}

		allocated = append(allocated, alloc)
		result.IPs = append(result.IPs, address)
	}

	if len(requested) != 0 {
		return errors.New("requested IPs do not belong to verified PodCIDRs")
	}

	printResult := p.printResult
	if printResult == nil {
		printResult = types.PrintResult
	}

	return printResult(result, confVersion)
}

func hostLocalConfig(config *ipamproto.Config, dataDir string) ([]byte, error) {
	if config == nil || len(config.Ranges) == 0 {
		return nil, errors.New("empty verified IPAM configuration")
	}

	type subnet struct {
		Subnet string `json:"subnet"`
	}

	var ranges [][]subnet

	for _, set := range config.Ranges {
		if set == nil || len(set.Ranges) == 0 {
			return nil, errors.New("empty verified IPAM range set")
		}

		var pool []subnet

		for _, r := range set.Ranges {
			if r == nil {
				return nil, errors.New("nil verified IPAM range")
			}

			if _, err := ConfigFromCIDRs([]string{r.Subnet}); err != nil {
				return nil, err
			}

			pool = append(pool, subnet{Subnet: r.Subnet})
		}

		ranges = append(ranges, pool)
	}

	return json.Marshal(struct {
		Type    string     `json:"type"`
		Ranges  [][]subnet `json:"ranges"`
		DataDir string     `json:"dataDir,omitempty"`
	}{Type: "host-local", Ranges: ranges, DataDir: dataDir})
}

func (p Plugin) Del(args *skel.CmdArgs) (returnErr error) {
	_, name, _, err := parseInput(args)
	if err != nil {
		return err
	}

	store, err := disk.New(name, p.DataDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()

	if err := store.Lock(); err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, store.Unlock()) }()

	return store.ReleaseByID(args.ContainerID, args.IfName)
}

func (p Plugin) Check(args *skel.CmdArgs) (returnErr error) {
	_, name, _, err := parseInput(args)
	if err != nil {
		return err
	}

	store, err := disk.New(name, p.DataDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()

	if !store.FindByID(args.ContainerID, args.IfName) {
		return fmt.Errorf("no IPAM reservation for container %s interface %s", args.ContainerID, args.IfName)
	}

	return nil
}
