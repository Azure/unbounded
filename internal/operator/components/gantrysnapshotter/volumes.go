// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantrysnapshotter

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

	// volumePrefix names the image volumes. The names are stable and derived
	// from the index, because a segment is addressed cluster-wide by the extent
	// ID it was allocated and losing track of which volume that was means
	// losing the layers in it.
	volumePrefix = "gantry-image"

	// defaultSegments is how many segment volumes are created when a Site does
	// not say. Layers are spread across segments so that a node writing one
	// layer does not serialise behind a node writing another.
	defaultSegments = 4

	// defaultSegmentBytes is the size of one segment volume. It has to be a
	// multiple of the 4 MiB page IMMUTABLE_4M is stored in.
	defaultSegmentBytes = 8 << 30

	// defaultCatalogBytes is the size of the catalog volume. The catalog is an
	// append-only record per layer, so it is small; it is sized generously
	// because it cannot be grown afterwards.
	defaultCatalogBytes = 256 << 20
)

// layout is the image volume's shape, resolved from a Site.
type layout struct {
	segments     int32
	segmentBytes int64
	catalogBytes int64
}

// layoutFor resolves the image volume's shape from the Site that enables the
// component, or from the defaults when none does.
//
// The sizes are validated here rather than left to the CSI geometry parser
// because a rejected geometry surfaces as a volume that is never placed, with
// the reason on the racer allocator's log line rather than on the Site.
func layoutFor(site *unboundedv1alpha3.Site) (layout, error) {
	out := layout{
		segments:     defaultSegments,
		segmentBytes: defaultSegmentBytes,
		catalogBytes: defaultCatalogBytes,
	}

	if site == nil || site.Spec.Components.GantrySnapshotter == nil {
		return out, nil
	}

	spec := site.Spec.Components.GantrySnapshotter

	if spec.Segments != nil {
		out.segments = *spec.Segments
	}

	if spec.SegmentSize != nil {
		out.segmentBytes = spec.SegmentSize.Value()
	}

	if spec.CatalogSize != nil {
		out.catalogBytes = spec.CatalogSize.Value()
	}

	if out.segments < 1 {
		return layout{}, fmt.Errorf("gantrySnapshotter.segments must be at least 1, got %d", out.segments)
	}

	// Both sizes are checked against the 4 MiB page rather than against their
	// own kind's page. A segment is a whole IMMUTABLE_4M extent, and the
	// catalog is a mutable head with nothing after it, which the geometry
	// parser still requires to end on the tail's alignment.
	for _, check := range []struct {
		name  string
		bytes int64
	}{
		{name: "segmentSize", bytes: out.segmentBytes},
		{name: "catalogSize", bytes: out.catalogBytes},
	} {
		if check.bytes <= 0 || check.bytes%racerctrl.HugePage != 0 {
			return layout{}, fmt.Errorf("gantrySnapshotter.%s must be a positive multiple of %d, got %d",
				check.name, racerctrl.HugePage, check.bytes)
		}
	}

	return out, nil
}

// desiredVolume describes one image volume.
type desiredVolume struct {
	name       string
	role       string
	bytes      int64
	attributes map[string]string
}

// desiredVolumes enumerates the image volume's members.
func desiredVolumes(l layout) []desiredVolume {
	out := make([]desiredVolume, 0, l.segments+1)

	// The catalog is a single mutable extent covering the whole device. OCC
	// rather than LWW because every node writes to it, and the records are
	// published with a compare-and-swap so that two nodes ingesting the same
	// layer cannot both claim the same catalog slot.
	out = append(out, desiredVolume{
		name:  volumePrefix + "-catalog",
		role:  racerctrl.ImageRoleCatalog,
		bytes: l.catalogBytes,
		attributes: map[string]string{
			racerctrl.AttrMutableBytes: fmt.Sprintf("%d", l.catalogBytes),
			racerctrl.AttrMutableKind:  "OCC",
		},
	})

	for i := int32(0); i < l.segments; i++ {
		// No mutable head at all, so the whole device is one IMMUTABLE_4M
		// extent. That is what lets a layer be written as whole 4 MiB pages and
		// read back with a device-mapper linear target rather than a copy.
		out = append(out, desiredVolume{
			name:  fmt.Sprintf("%s-segment-%d", volumePrefix, i),
			role:  racerctrl.ImageRoleSegment,
			bytes: l.segmentBytes,
			attributes: map[string]string{
				racerctrl.AttrImmutablePageSize: "4Mi",
			},
		})
	}

	return out
}

// ensureImageVolumes creates any missing image volumes and reports what is
// still waiting to be usable.
//
// It creates and never updates. A racer volume's extent layout is frozen when
// the allocator places it, so a volume whose size no longer matches the Site
// cannot be brought into line by writing to it; saying so is the only honest
// thing to do.
func ensureImageVolumes(ctx context.Context, env *component.Env, l layout) (string, error) {
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

	var (
		waiting  []string
		mismatch []string
	)

	for _, want := range desiredVolumes(l) {
		existing := &corev1.PersistentVolume{}

		getErr := env.Client.Get(ctx, client.ObjectKey{Name: want.name}, existing)
		switch {
		case apierrors.IsNotFound(getErr):
			if err := env.Client.Create(ctx, buildVolume(env, want)); err != nil && !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("create image volume %s: %w", want.name, err)
			}

			waiting = append(waiting, want.name)

			continue
		case getErr != nil:
			return "", fmt.Errorf("get image volume %s: %w", want.name, getErr)
		}

		if capacity, ok := existing.Spec.Capacity[corev1.ResourceStorage]; ok && capacity.Value() != want.bytes {
			mismatch = append(mismatch, fmt.Sprintf("%s is %s, not %s",
				want.name, capacity.String(), resource.NewQuantity(want.bytes, resource.BinarySI).String()))
		}

		if existing.Annotations[racerctrl.CompositionAnnotation] == "" {
			waiting = append(waiting, want.name)
		}
	}

	sort.Strings(waiting)
	sort.Strings(mismatch)

	switch {
	case len(mismatch) > 0:
		// Reported, not corrected: an extent's page count is frozen for its
		// life, so the only way to resize is to create a new volume and lose
		// what is in the old one.
		return "image volumes cannot be resized in place: " + strings.Join(mismatch, ", "), nil
	case len(waiting) > 0:
		return "waiting for the racer allocator to place: " + strings.Join(waiting, ", "), nil
	}

	return "", nil
}

// buildVolume renders one image volume.
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
				racerctrl.ImageRoleAnnotation: want.role,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(want.bytes, resource.BinarySI),
			},
			// Every racer node exports every image volume at once, which is
			// what many-reader access to a shared layer store means. No pod
			// ever mounts one, so this is documentation rather than
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
