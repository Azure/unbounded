package netboot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/casext/mediatype"
	"github.com/opencontainers/umoci/oci/layer"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

// Docker media types that are structurally compatible with OCI equivalents.
const (
	dockerMediaTypeManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerMediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerMediaTypeConfig       = "application/vnd.docker.container.image.v1+json"
	dockerMediaTypeLayer        = "application/vnd.docker.image.rootfs.diff.tar"
	dockerMediaTypeLayerGzip    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	dockerMediaTypeForeignLayer = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)

func init() {
	// Register parsers for Docker V2 media types so that umoci's
	// FromDescriptor returns parsed Go structs (ispec.Manifest, ispec.Index,
	// ispec.Image) instead of raw readers.  Docker V2 manifests and manifest
	// lists are structurally compatible with their OCI counterparts, so
	// simple JSON decoding into the OCI types is sufficient.
	mediatype.RegisterTarget(dockerMediaTypeManifest)
	mediatype.RegisterParser(dockerMediaTypeManifest, func(rdr io.Reader) (any, error) {
		var m ispec.Manifest
		if rdr == nil {
			return m, nil
		}

		if err := json.NewDecoder(rdr).Decode(&m); err != nil {
			return nil, fmt.Errorf("decode Docker manifest: %w", err)
		}

		return m, nil
	})

	mediatype.RegisterParser(dockerMediaTypeManifestList, func(rdr io.Reader) (any, error) {
		var idx ispec.Index
		if rdr == nil {
			return idx, nil
		}

		if err := json.NewDecoder(rdr).Decode(&idx); err != nil {
			return nil, fmt.Errorf("decode Docker manifest list: %w", err)
		}

		return idx, nil
	})

	mediatype.RegisterParser(dockerMediaTypeConfig, func(rdr io.Reader) (any, error) {
		var img ispec.Image
		if rdr == nil {
			return img, nil
		}

		if err := json.NewDecoder(rdr).Decode(&img); err != nil {
			return nil, fmt.Errorf("decode Docker image config: %w", err)
		}

		return img, nil
	})
}

// imageResyncInterval is how often the reconciler re-resolves remote tags
// to detect updated images pushed under the same tag.
const imageResyncInterval = 5 * time.Minute

// OCIReconciler watches Machine CRs and pulls their referenced OCI images.
// Work items are deduplicated by image reference so that multiple machines
// sharing the same image only trigger a single download.
type OCIReconciler struct {
	Client client.Client
	Cache  *OCICache
}

func (r *OCIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("oci-image").
		Watches(&v1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineToImage)).
		Complete(r)
}

// mapMachineToImage maps a Machine event to a reconcile request keyed by
// image reference. This ensures that multiple machines referencing the same
// image produce only one work item in the queue.
func (r *OCIReconciler) mapMachineToImage(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*v1alpha3.Machine)
	if !ok {
		return nil
	}

	if machine.Spec.PXE == nil || machine.Spec.PXE.Image == "" {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: client.ObjectKey{Name: machine.Spec.PXE.Image}},
	}
}

func (r *OCIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// req.Name is the OCI image reference, mapped from Machine events.
	imageRef := req.Name

	// Always resolve the remote digest so we detect tag updates.
	remoteDigest, repo, err := r.resolveRemoteDigest(ctx, imageRef)
	if err != nil {
		logger.Error(err, "resolving OCI image digest", "image", imageRef)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if we already have this exact digest cached.
	existingDigest := r.Cache.DigestFor(imageRef)
	if existingDigest == remoteDigest && r.Cache.IsCached(remoteDigest) {
		return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
	}

	logger.Info("pulling OCI image", "image", imageRef, "digest", remoteDigest)

	if err := r.pullAndUnpack(ctx, imageRef, remoteDigest, repo); err != nil {
		logger.Error(err, "pulling OCI image", "image", imageRef)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.Cache.SetDigest(imageRef, remoteDigest)
	logger.Info("OCI image cached", "image", imageRef, "digest", remoteDigest)

	return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
}

// newRepository creates a remote.Repository for the given image reference,
// configuring plain HTTP for localhost registries.
func newRepository(imageRef string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}

	// Use plain HTTP for localhost registries (common in development and testing).
	host := repo.Reference.Host()
	if host == "localhost" || strings.HasPrefix(host, "localhost:") ||
		host == "127.0.0.1" || strings.HasPrefix(host, "127.0.0.1:") {
		repo.PlainHTTP = true
	}

	return repo, nil
}

// resolveRemoteDigest resolves the tag or digest in an image reference to its
// canonical digest by querying the remote registry.
func (r *OCIReconciler) resolveRemoteDigest(ctx context.Context, imageRef string) (string, *remote.Repository, error) {
	repo, err := newRepository(imageRef)
	if err != nil {
		return "", nil, err
	}

	tagOrDigest := repo.Reference.Reference

	desc, err := repo.Resolve(ctx, tagOrDigest)
	if err != nil {
		return "", nil, fmt.Errorf("resolving image %q: %w", imageRef, err)
	}

	return desc.Digest.String(), repo, nil
}

func (r *OCIReconciler) pullAndUnpack(ctx context.Context, imageRef, imageDigest string, repo *remote.Repository) error {
	// Check if already cached (another reconcile may have beaten us).
	if r.Cache.IsCached(imageDigest) {
		return nil
	}

	imageDir := r.Cache.ImageDir(imageDigest)

	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("creating image dir: %w", err)
	}

	tagOrDigest := repo.Reference.Reference

	// Create a temporary directory for the OCI layout store.
	layoutDir, err := os.MkdirTemp("", "metalman-oci-*")
	if err != nil {
		return fmt.Errorf("create temp dir for OCI layout: %w", err)
	}
	defer os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup

	store, err := oci.New(layoutDir)
	if err != nil {
		return fmt.Errorf("create OCI layout store: %w", err)
	}

	// Copy (pull) the image from the remote repository into the local OCI layout.
	if _, err := oras.Copy(ctx, repo, tagOrDigest, store, tagOrDigest, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("pull image %q: %w", imageRef, err)
	}

	// Unpack the OCI layout into the image directory using umoci.
	if err := unpackOCILayout(ctx, layoutDir, tagOrDigest, imageDir); err != nil {
		os.RemoveAll(imageDir) //nolint:errcheck // Clean up partial unpack.
		return fmt.Errorf("unpack OCI image: %w", err)
	}

	// Verify /disk/ directory exists (kubevirt containerDisk convention).
	diskDir := r.Cache.DiskDir(imageDigest)
	if _, err := os.Stat(diskDir); err != nil {
		os.RemoveAll(imageDir) //nolint:errcheck // Clean up partial unpack.
		return fmt.Errorf("OCI image missing /disk directory")
	}

	return nil
}

// convertDockerMediaTypes rewrites Docker V2 media types in a manifest to
// their OCI equivalents in-place.  Docker V2 and OCI blobs are structurally
// identical; only the MIME types differ.  umoci's UnpackRootfs strictly
// checks for OCI media types, so this conversion is required when pulling
// images produced by `docker build`.
func convertDockerMediaTypes(m *ispec.Manifest) {
	// Config blob.
	switch m.Config.MediaType {
	case dockerMediaTypeConfig:
		m.Config.MediaType = ispec.MediaTypeImageConfig
	}

	// Layer blobs.
	for i := range m.Layers {
		switch m.Layers[i].MediaType {
		case dockerMediaTypeLayerGzip:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayerGzip
		case dockerMediaTypeLayer:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayer
		case dockerMediaTypeForeignLayer:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayerNonDistributableGzip //nolint:staticcheck // matching deprecated OCI type
		}
	}

	// Manifest media type itself (stored in the descriptor that points to
	// this manifest — not inside the manifest JSON — but some tools also
	// set MediaType inside the manifest body).
	switch m.MediaType {
	case dockerMediaTypeManifest:
		m.MediaType = ispec.MediaTypeImageManifest
	}
}

// unpackOCILayout opens an OCI image layout at layoutDir and unpacks the
// image tagged with the given tag into destDir using umoci. It picks the
// first available manifest (netboot images are single-platform).
func unpackOCILayout(ctx context.Context, layoutDir, tag, destDir string) error {
	engine, err := umoci.OpenLayout(layoutDir)
	if err != nil {
		return fmt.Errorf("open OCI layout %q: %w", layoutDir, err)
	}
	defer engine.Close() //nolint:errcheck // best effort close

	descriptorPaths, err := engine.ResolveReference(ctx, tag)
	if err != nil {
		return fmt.Errorf("resolve tag %q: %w", tag, err)
	}

	if len(descriptorPaths) == 0 {
		return fmt.Errorf("tag %q not found in OCI layout", tag)
	}

	// Use the first descriptor — netboot images are single-platform.
	dp := descriptorPaths[0]

	blob, err := engine.FromDescriptor(ctx, dp.Descriptor())
	if err != nil {
		return fmt.Errorf("read manifest blob for tag %q: %w", tag, err)
	}
	defer blob.Close() //nolint:errcheck // best effort close

	manifest, ok := blob.Data.(ispec.Manifest)
	if !ok {
		return fmt.Errorf("tag %q does not point to an OCI manifest (got %T)", tag, blob.Data)
	}

	// Convert Docker media types to OCI equivalents so that umoci's strict
	// media-type checks pass. Docker V2 images use different MIME types for
	// the config and layer blobs but are structurally identical to OCI.
	convertDockerMediaTypes(&manifest)

	unpackOpts := &layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{
			MapOptions: layer.MapOptions{
				Rootless: true,
			},
		},
	}

	if err := layer.UnpackRootfs(ctx, engine, destDir, manifest, unpackOpts); err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
	}

	return nil
}
