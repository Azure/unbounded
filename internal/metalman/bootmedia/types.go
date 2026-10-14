// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootmedia

import (
	"context"
	"strings"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Source identifies a bootstrap ISO served to a management controller over TFTP.
type Source struct {
	Name   string
	Server string
}

// PrimerName returns a second name for the same ISO, or "" when name has no
// reserved prefix. Some managers attach a readable drive only after a mount
// under a different name, so clients may mount this name first. Tested rack
// manager firmware showed this after power cycles that followed a media boot.
func PrimerName(name string) string {
	rest, ok := strings.CutPrefix(name, "ub-")
	if !ok {
		return ""
	}

	return "up-" + rest
}

// Provider builds immutable, operation-owned media before any power changes.
type Provider interface {
	Prepare(context.Context, *v1alpha3.Machine, *v1alpha3.MachineOperation, string) (Source, error)
	Release(context.Context, string) error
}

// Client is the optional virtual-media capability of a power client.
type Client interface {
	SupportsVirtualMedia() bool
	StageVirtualMedia(context.Context, Source) error
	MountVirtualMedia(context.Context, Source) error
	SetVirtualMediaBoot(context.Context) error
	CleanupVirtualMedia(context.Context, Source) error
	DeleteVirtualMedia(context.Context, Source) error
}
