// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type installConfig struct {
	ImageURL   string
	ServeURL   string
	TargetDisk string
	BootMAC    string
	IPParam    string
	CloudInit  cloudInitConfig
}

func (i *Installer) readInstallConfig() (installConfig, error) {
	cmdlineBytes, err := os.ReadFile(i.ProcCmdline)
	if err != nil {
		return installConfig{}, fmt.Errorf("reading kernel command line: %w", err)
	}

	cfg, err := installConfigFromCmdline(string(cmdlineBytes))
	if err != nil {
		return installConfig{}, err
	}

	return cfg, nil
}

func installConfigFromCmdline(cmdline string) (installConfig, error) {
	params := parseCmdline(cmdline)

	imageURL := params["unbounded.image_url"]
	if imageURL == "" {
		return installConfig{}, errors.New("unbounded.image_url not set")
	}

	bootMAC := normalizeMAC(params["unbounded.boot_mac"])
	if bootMAC == "" && params["BOOTIF"] != "" {
		bootMAC = bootifToMAC(params["BOOTIF"])
	}

	serveURL := params["unbounded.serve_url"]

	return installConfig{
		ImageURL:   imageURL,
		ServeURL:   serveURL,
		TargetDisk: params["unbounded.disk"],
		BootMAC:    bootMAC,
		IPParam:    params["ip"],
		CloudInit: cloudInitConfig{
			DSURL:         params["unbounded.ds_url"],
			ServeURL:      serveURL,
			NodeName:      params["unbounded.node_name"],
			NodeNamespace: params["unbounded.node_namespace"],
			APIServerURL:  params["unbounded.apiserver_url"],
		},
	}, nil
}

func parseCmdline(cmdline string) map[string]string {
	params := make(map[string]string)

	for _, tok := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}

		params[key] = value
	}

	return params
}

func normalizeMAC(mac string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mac)), "-", ":")
}

func bootifToMAC(bootif string) string {
	value := strings.ToLower(strings.TrimSpace(bootif))
	value = strings.TrimPrefix(value, "01-")

	return strings.ReplaceAll(value, "-", ":")
}
