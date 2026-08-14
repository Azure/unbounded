// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantrysnapshotter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerctrl"
)

const (
	// storageClassName is the racer universe the image volume lives in. It is
	// the default class the racer component creates; the image volume has no
	// reason to be in a universe of its own, and being in the default one means
	// it shares the catalog and the gateway set that is already there.
	storageClassName = "racer"

	// volumeName is the image volume. There is exactly one, and its name is
	// fixed: the layers in it are addressed by the extent ids it was allocated,
	// so losing track of which object those belong to means losing them.
	volumeName = "gantry-image"

	// defaultSizeBytes is the usable capacity for layer bytes when a Site does
	// not say.
	defaultSizeBytes = 32 << 30

	// defaultExtentBytes is the size of one image extent. It has to be a
	// multiple of the 4 MiB page IMMUTABLE_4M is stored in, and it is the unit
	// the cleaner reclaims, because racer collects a whole extent at a time.
	defaultExtentBytes = 8 << 30

	// defaultCatalogBytes is the size of the catalog extent. The catalog is an
	// append-only record per layer, so it is small; it is sized generously
	// because it cannot be grown afterwards.
	defaultCatalogBytes = 256 << 20
)

// layout is the image volume's shape, resolved from a Site.
type layout struct {
	sizeBytes    int64
	extentBytes  int64
	catalogBytes int64
}

// capacity is the whole volume: the catalog head plus the layer space after it.
func (l layout) capacity() int64 { return l.catalogBytes + l.sizeBytes }

// layoutFor resolves the image volume's shape from the Site that enables the
// component, or from the defaults when none does.
//
// The sizes are validated here rather than left to the CSI geometry parser
// because a rejected geometry surfaces as a volume that is never placed, with
// the reason on the racer allocator's log line rather than on the Site.
func layoutFor(site *unboundedv1alpha3.Site) (layout, error) {
	out := layout{
		sizeBytes:    defaultSizeBytes,
		extentBytes:  defaultExtentBytes,
		catalogBytes: defaultCatalogBytes,
	}

	if site != nil && site.Spec.Components.GantrySnapshotter != nil {
		spec := site.Spec.Components.GantrySnapshotter

		if spec.Size != nil {
			out.sizeBytes = spec.Size.Value()
		}

		if spec.ExtentSize != nil {
			out.extentBytes = spec.ExtentSize.Value()
		}

		if spec.CatalogSize != nil {
			out.catalogBytes = spec.CatalogSize.Value()
		}
	}

	// All three are checked against the 4 MiB page rather than against their
	// own kind's page. The layer extents are IMMUTABLE_4M, and the catalog is
	// a mutable head, which the geometry parser requires to end on the tail's
	// alignment so that the first layer extent starts on a page boundary.
	for _, check := range []struct {
		name  string
		bytes int64
	}{
		{name: "size", bytes: out.sizeBytes},
		{name: "extentSize", bytes: out.extentBytes},
		{name: "catalogSize", bytes: out.catalogBytes},
	} {
		if check.bytes <= 0 || check.bytes%racerctrl.HugePage != 0 {
			return layout{}, fmt.Errorf("gantrySnapshotter.%s must be a positive multiple of %d, got %d",
				check.name, racerctrl.HugePage, check.bytes)
		}
	}

	// The space is cut into extents of exactly extentSize, because an extent's
	// page count is frozen when it is allocated and a short one at the end
	// would be a segment the cleaner treats as the same size as the others.
	if out.sizeBytes%out.extentBytes != 0 {
		return layout{}, fmt.Errorf("gantrySnapshotter.size %d must be a multiple of extentSize %d",
			out.sizeBytes, out.extentBytes)
	}

	if extents := out.sizeBytes / out.extentBytes; extents > racerctrl.MaxVolumeExtents {
		return layout{}, fmt.Errorf(
			"gantrySnapshotter.size %d over extentSize %d needs %d extents, more than the %d a volume may have",
			out.sizeBytes, out.extentBytes, extents, racerctrl.MaxVolumeExtents)
	}

	return out, nil
}

// desiredVolume describes the image volume.
type desiredVolume struct {
	name       string
	bytes      int64
	attributes map[string]string
}

// desiredImageVolume renders the image volume's geometry.
//
// One volume, whose composition is an OCC catalog extent followed by the
// IMMUTABLE_4M extents layer bytes are written into. OCC rather than LWW for
// the catalog because every node writes to it and records are published with a
// compare-and-swap, so two nodes ingesting the same layer cannot both claim the
// same slot. The tail carries no mutable head of its own, which is what lets a
// layer be written as whole 4 MiB pages and read back with a device-mapper
// linear target rather than a copy.
func desiredImageVolume(l layout) desiredVolume {
	return desiredVolume{
		name:  volumeName,
		bytes: l.capacity(),
		attributes: map[string]string{
			racerctrl.AttrMutableBytes:         fmt.Sprintf("%d", l.catalogBytes),
			racerctrl.AttrMutableKind:          "OCC",
			racerctrl.AttrImmutablePageSize:    "4Mi",
			racerctrl.AttrImmutableExtentBytes: fmt.Sprintf("%d", l.extentBytes),
		},
	}
}

// ensureImageVolume creates the image volume if it is missing and reports
// whether it is still waiting to be usable.
//
// It creates and never updates. A racer volume's extent layout is frozen when
// the allocator places it, so a volume whose size no longer matches the Site
// cannot be brought into line by writing to it; saying so is the only honest
// thing to do.
func ensureImageVolume(ctx context.Context, env *component.Env, l layout) (string, error) {
	// The class is a racer universe. Without it there is nothing to allocate
	// extents out of, and creating volumes that name a class that does not
	// exist would just move the wait somewhere less visible.
	err := env.Client.Get(ctx, client.ObjectKey{Name: storageClassName}, &storagev1.StorageClass{})
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("waiting for the racer StorageClass %q", storageClassName), nil
	}

	if err != nil {
		return "", fmt.Errorf("get StorageClass %s: %w", storageClassName, err)
	}

	want := desiredImageVolume(l)
	existing := &corev1.PersistentVolume{}

	getErr := env.Client.Get(ctx, client.ObjectKey{Name: want.name}, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		if err := env.Client.Create(ctx, buildVolume(env, want)); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create image volume %s: %w", want.name, err)
		}

		return "waiting for the racer allocator to place " + want.name, nil
	case getErr != nil:
		return "", fmt.Errorf("get image volume %s: %w", want.name, getErr)
	}

	if capacity, ok := existing.Spec.Capacity[corev1.ResourceStorage]; ok && capacity.Value() != want.bytes {
		// Reported, not corrected: an extent's page count is frozen for its
		// life, so the only way to resize is to create a new volume and lose
		// what is in the old one.
		return fmt.Sprintf("the image volume cannot be resized in place: %s is %s, not %s",
			want.name, capacity.String(),
			resource.NewQuantity(want.bytes, resource.BinarySI).String()), nil
	}

	if existing.Annotations[racerctrl.CompositionAnnotation] == "" {
		return "waiting for the racer allocator to place " + want.name, nil
	}

	return "", nil
}

// buildVolume renders the image volume.
func buildVolume(env *component.Env, want desiredVolume) *corev1.PersistentVolume {
	reclaim := corev1.PersistentVolumeReclaimRetain
	mode := corev1.PersistentVolumeBlock

	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: want.name,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "gantry-snapshotter",
				"app.kubernetes.io/component": "image-volume",
			},
			Annotations: map[string]string{
				racerctrl.ImageRoleAnnotation: racerctrl.ImageRoleImage,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(want.bytes, resource.BinarySI),
			},
			// Every racer node exports the image volume at once, which is
			// what many-reader access to a shared layer store means. No pod
			// ever mounts it, so this is documentation rather than
			// enforcement.
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: reclaim,
			StorageClassName:              storageClassName,
			VolumeMode:                    &mode,
			// Reserved for a claim that is never created. A racer PV with no
			// claimRef is Available, and the binder will happily hand it to the
			// next PersistentVolumeClaim that asks for this class - which would
			// silently give a workload the cluster's layer store. An empty-UID
			// claimRef is the documented way to say "this one is spoken for".
			ClaimRef: &corev1.ObjectReference{
				Kind:      "PersistentVolumeClaim",
				Namespace: env.Namespace,
				Name:      want.name,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           racerctrl.DriverName,
					VolumeHandle:     want.name,
					VolumeAttributes: want.attributes,
				},
			},
		},
	}
}
