// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command gantry-snapshotter runs the RACER-backed containerd snapshotter.
//
// containerd talks to it as a proxy plugin over a unix socket. Image layers
// live once per cluster as uncompressed EROFS images inside RACER extents, so
// a node that starts a pod maps and mounts them instead of downloading and
// unpacking them. See designs/gantry-snapshotter-design.md.
//
// Subcommands:
//
//	gantry-snapshotter version   print build information and exit
//	gantry-snapshotter [flags]   run the daemon
package main
