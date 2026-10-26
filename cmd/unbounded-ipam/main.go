// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/Azure/unbounded/internal/net/ipam"
	buildversion "github.com/Azure/unbounded/internal/version"
)

func main() {
	plugin := ipam.Plugin{}
	skel.PluginMainFuncs(plugin.Functions(), version.All, "unbounded-ipam "+buildversion.Version)
}
