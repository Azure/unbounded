// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type reclaimFakeRuntime struct {
	images       []reclaimImage
	containers   []string
	leases       int
	deleted      []string
	listErr      error
	containerErr error
	leaseErr     error
	deleteErr    error
	keepAliases  bool
	listCalls    int
	onList       func(*reclaimFakeRuntime)
}

func (r *reclaimFakeRuntime) listImages(context.Context) ([]reclaimImage, error) {
	r.listCalls++
	if r.onList != nil {
		r.onList(r)
	}

	return append([]reclaimImage(nil), r.images...), r.listErr
}

func (r *reclaimFakeRuntime) listContainerImages(context.Context) ([]string, error) {
	return r.containers, r.containerErr
}

func (r *reclaimFakeRuntime) countLeases(context.Context) (int, error) {
	return r.leases, r.leaseErr
}

func (r *reclaimFakeRuntime) deleteImage(_ context.Context, image reclaimImage) error {
	r.deleted = append(r.deleted, image.name)
	if !r.keepAliases && (r.deleteErr == nil || errdefs.IsNotFound(r.deleteErr)) {
		for i, existing := range r.images {
			if existing.name == image.name && existing.target.Digest == image.target.Digest {
				r.images = append(r.images[:i], r.images[i+1:]...)
				break
			}
		}
	}

	return r.deleteErr
}

func reclaimTestImage(name, identity string) reclaimImage {
	return reclaimImage{
		name: name, config: digest.FromString(identity + "-config").String(),
		target: ocispec.Descriptor{Digest: digest.FromString(identity)},
	}
}

func reclaimTestUsage(context.Context) (uint64, error) { return 100, nil }

func TestReclaimWaitsForDeletedDemoRuntimeReferences(t *testing.T) {
	demo := reclaimTestImage("registry/demo:deleted", "deleted")
	alias := demo
	alias.name = demo.config
	unrelated := reclaimTestImage("registry/system:running", "system")
	runtime := &reclaimFakeRuntime{
		images:     []reclaimImage{demo, alias, unrelated},
		containers: []string{demo.config, unrelated.config}, leases: 4,
	}
	runtime.onList = func(r *reclaimFakeRuntime) {
		if r.listCalls >= 6 {
			r.containers = []string{unrelated.config}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := reclaimNode(ctx, ReclaimNodeResult{}, runtime, reclaimTestUsage, demo.target.Digest.String())
	if err != nil || result.Images != 1 || len(runtime.deleted) != 2 || result.ProtectedLeases != 4 ||
		len(runtime.images) != 1 || runtime.images[0].config != unrelated.config {
		t.Fatalf("delayed runtime removal was not reclaimed safely: %+v deleted=%v err=%v", result, runtime.deleted, err)
	}
}

func TestReclaimReportsRetainedDeletedDemoImages(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "container reference", true: "pinned"}[pinned], func(t *testing.T) {
			demo := reclaimTestImage("registry/demo:deleted", "deleted")
			demo.pinned = pinned
			runtime := &reclaimFakeRuntime{images: []reclaimImage{demo}, containers: []string{demo.config}, leases: 3}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			result, err := reclaimNode(ctx, ReclaimNodeResult{}, runtime, reclaimTestUsage, demo.target.Digest.String())
			if err == nil || result.Images != 0 || len(runtime.deleted) != 0 || result.ProtectedLeases != 3 {
				t.Fatalf("protected deleted demo reported success: %+v err=%v", result, err)
			}

			if !pinned && (!errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "runtime references")) {
				t.Fatalf("missing retained-image diagnostic: %v", err)
			}
		})
	}
}

func TestReclaimDeletedDemoAlreadyAbsent(t *testing.T) {
	other := reclaimTestImage("registry/system:running", "system")
	runtime := &reclaimFakeRuntime{images: []reclaimImage{other}, containers: []string{other.config}}

	result, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage, digest.FromString("deleted").String())
	if err != nil || result.Images != 0 || len(runtime.deleted) != 0 {
		t.Fatalf("absent demo changed unrelated images: %+v err=%v", result, err)
	}
}

func TestReclaimRuntimeProtectsContainersAndPinnedImages(t *testing.T) {
	base := reclaimTestImage("registry/demo:one", "one")

	for _, tt := range []struct {
		name string
		ref  string
	}{
		{name: "running or stopped config identity", ref: base.config},
		{name: "tag reference", ref: base.name},
		{name: "manifest identity", ref: base.target.Digest.String()},
		{name: "qualified manifest", ref: "registry/demo@" + base.target.Digest.String()},
		{name: "qualified config", ref: "registry/demo@" + base.config},
		{name: "missing container metadata"},
		{name: "unresolved stopped container", ref: "registry/gone:old"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			alias := base
			alias.name = base.config
			runtime := &reclaimFakeRuntime{images: []reclaimImage{base, alias}, containers: []string{tt.ref}}

			result, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)

			wantError := tt.ref == "" || tt.ref == "registry/gone:old"
			if (err != nil) != wantError || result.Images != 0 || len(runtime.deleted) != 0 {
				t.Fatalf("protected container image was changed: result=%+v deleted=%v err=%v", result, runtime.deleted, err)
			}
		})
	}

	t.Run("pinned alias protects all aliases and sandbox image", func(t *testing.T) {
		pinned := base
		pinned.name, pinned.pinned = base.config, true
		runtime := &reclaimFakeRuntime{images: []reclaimImage{base, pinned}, leases: 2}

		result, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)
		if err != nil || result.Images != 0 || len(runtime.deleted) != 0 || result.ProtectedLeases != 2 {
			t.Fatalf("pinned image changed: result=%+v err=%v", result, err)
		}
	})
}

func TestReclaimRuntimeRemovesEveryAliasAndKeepsLeases(t *testing.T) {
	base := reclaimTestImage("registry/demo:one", "one")
	alias := base
	alias.name = base.config
	other := reclaimTestImage("registry/system:one", "system")
	runtime := &reclaimFakeRuntime{
		images: []reclaimImage{base, alias, other}, containers: []string{other.config}, leases: 7,
	}
	calls := 0
	used := func(context.Context) (uint64, error) {
		calls++
		return uint64(1000 / calls), nil
	}

	result, err := reclaimNode(context.Background(), ReclaimNodeResult{Node: "worker"}, runtime, used)
	if err != nil || result.Images != 1 || result.Leases != 0 || result.ProtectedLeases != 7 ||
		result.Before != 1000 || result.After != 500 || result.Node != "worker" {
		t.Fatalf("unexpected result: %+v err=%v", result, err)
	}

	if len(runtime.deleted) != 2 || len(runtime.images) != 1 || runtime.images[0].name != other.name {
		t.Fatalf("incorrect aliases removed: deleted=%v remaining=%v", runtime.deleted, runtime.images)
	}
}

func TestReclaimRuntimeVerifiesAbsenceAndBoundsAttempts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		deleteErr  error
		keep       bool
		wantErr    bool
		wantImages int
		wantCalls  int
	}{
		{name: "successful removal", wantImages: 1, wantCalls: 1},
		{name: "not found race", deleteErr: errdefs.ErrNotFound, wantImages: 1, wantCalls: 1},
		{name: "successful no-op is not absence", keep: true, wantErr: true, wantCalls: 2},
		{name: "not found with remaining alias", deleteErr: errdefs.ErrNotFound, keep: true, wantErr: true, wantCalls: 2},
		{name: "permission denied", deleteErr: errdefs.ErrPermissionDenied, wantErr: true, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &reclaimFakeRuntime{
				images:    []reclaimImage{reclaimTestImage("registry/demo:one", "one")},
				deleteErr: tt.deleteErr, keepAliases: tt.keep,
			}

			result, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)
			if (err != nil) != tt.wantErr || result.Images != tt.wantImages || len(runtime.deleted) != tt.wantCalls {
				t.Fatalf("result=%+v deleted=%v err=%v", result, runtime.deleted, err)
			}
		})
	}
}

func TestReclaimRuntimeRefreshesProtectionAndAliases(t *testing.T) {
	base := reclaimTestImage("registry/demo:one", "one")
	runtime := &reclaimFakeRuntime{images: []reclaimImage{base}}
	runtime.onList = func(r *reclaimFakeRuntime) {
		if r.listCalls == 2 {
			r.containers = []string{base.config}
		}
	}

	result, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)
	if err != nil || result.Images != 0 || len(runtime.deleted) != 0 {
		t.Fatalf("new container was not protected: %+v %v", result, err)
	}

	runtime = &reclaimFakeRuntime{images: []reclaimImage{base}}
	runtime.onList = func(r *reclaimFakeRuntime) {
		if r.listCalls == 3 {
			alias := base
			alias.name = "registry/demo:new-alias"
			r.images = append(r.images, alias)
		}
	}

	result, err = reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)
	if err != nil || result.Images != 1 || len(runtime.deleted) != 2 || len(runtime.images) != 0 {
		t.Fatalf("new alias was not verified: %+v deleted=%v err=%v", result, runtime.deleted, err)
	}
}

func TestReclaimRuntimeErrorsFailClosed(t *testing.T) {
	failure := errors.New("runtime unavailable")

	for _, tt := range []struct {
		name string
		set  func(*reclaimFakeRuntime)
	}{
		{name: "image inventory", set: func(r *reclaimFakeRuntime) { r.listErr = failure }},
		{name: "container inventory", set: func(r *reclaimFakeRuntime) { r.containerErr = failure }},
		{name: "lease inventory", set: func(r *reclaimFakeRuntime) { r.leaseErr = failure }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &reclaimFakeRuntime{images: []reclaimImage{reclaimTestImage("demo", "one")}}
			tt.set(runtime)

			_, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, reclaimTestUsage)
			if !errors.Is(err, failure) || len(runtime.deleted) != 0 {
				t.Fatalf("failure did not protect runtime: deleted=%v err=%v", runtime.deleted, err)
			}
		})
	}

	t.Run("storage measurement", func(t *testing.T) {
		runtime := &reclaimFakeRuntime{}

		_, err := reclaimNode(context.Background(), ReclaimNodeResult{}, runtime, func(context.Context) (uint64, error) {
			return 0, failure
		})
		if !errors.Is(err, failure) || runtime.listCalls != 0 {
			t.Fatalf("measurement failure did not prevent mutation: %v", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		runtime := &reclaimFakeRuntime{images: []reclaimImage{reclaimTestImage("demo", "one")}}

		_, err := reclaimNode(ctx, ReclaimNodeResult{}, runtime, reclaimTestUsage)
		if !errors.Is(err, context.Canceled) || len(runtime.deleted) != 0 {
			t.Fatalf("canceled operation mutated runtime: %v", err)
		}
	})
}

func TestReclaimNodeRequiresExplicitPaths(t *testing.T) {
	_, err := ReclaimNode(context.Background(), "worker", "", "/host-root", nil)
	if err == nil || !strings.Contains(err.Error(), "containerd socket") {
		t.Fatalf("missing socket accepted: %v", err)
	}
}
