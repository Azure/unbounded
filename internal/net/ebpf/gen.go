// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ebpf

// Generate Go bindings for the BPF program in bpf/unbounded_encap.c via the
// cilium/ebpf bpf2go tool. The version of bpf2go is resolved through go.mod
// (we go run from the same cilium/ebpf module already in our dependencies),
// so the generated bindings always match the runtime loader.
//
// Prerequisites for regenerating:
//   - clang in $PATH (Debian/Ubuntu: apt-get install clang)
//   - libbpf headers under /usr/include/bpf (Debian/Ubuntu: libbpf-dev)
//
// To regenerate, from the repository root run:
//   go generate ./internal/net/ebpf/...
//
// The generated files (*_bpfel.go and *_bpfel.o) are committed alongside
// the source so a normal `go build` does not require clang.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel -cc clang -cflags "-O2 -g -Wall -D__TARGET_ARCH_x86" unboundedEncap ../../../bpf/unbounded_encap.c
