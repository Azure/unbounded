// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	cplatforms "github.com/containerd/platforms"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"
	corev1 "k8s.io/api/core/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/ociutil"
)

func init() {
	ociutil.RegisterDockerParsers()
}

// imageResyncInterval is how often the reconciler re-resolves remote tags
// to detect updated images pushed under the same tag.
const imageResyncInterval = 5 * time.Minute

// OCIReconciler watches Machine CRs and pulls their referenced OCI images.
// Work items are deduplicated by image reference, architecture, and pull secret
// so machines sharing the same pull identity only trigger one download.
type OCIReconciler struct {
	Client                      client.Client
	Cache                       *OCICache
	DefaultNetbootRef           string
	DefaultNetbootPullSecretRef *v1alpha3.NamespacedSecretReference
}

type imagePullRequest struct {
	ImageRef      string                              `json:"imageRef"`
	Architecture  string                              `json:"architecture"`
	PullSecretRef *v1alpha3.NamespacedSecretReference `json:"pullSecretRef,omitempty"`
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

	if machine.Spec.PXE == nil {
		return nil
	}

	refs := []imagePullRequest{
		{
			ImageRef:      machine.Spec.PXE.Image,
			Architecture:  machine.Spec.PXE.TargetArchitecture(),
			PullSecretRef: machine.Spec.PXE.PullSecretRef,
		},
	}

	if machine.Spec.PXE.NetbootImage != "" {
		refs = append(refs, imagePullRequest{
			ImageRef:      machine.Spec.PXE.NetbootImage,
			Architecture:  machine.Spec.PXE.TargetArchitecture(),
			PullSecretRef: machine.Spec.PXE.NetbootPullSecretRef,
		})
	} else {
		refs = append(refs, imagePullRequest{
			ImageRef:      r.DefaultNetbootRef,
			Architecture:  machine.Spec.PXE.TargetArchitecture(),
			PullSecretRef: r.DefaultNetbootPullSecretRef,
		})
	}

	reqs := make([]reconcile.Request, 0, len(refs))

	seen := make(map[string]struct{}, len(refs))

	for _, pullReq := range refs {
		if pullReq.ImageRef == "" {
			continue
		}

		key, err := encodeImagePullRequest(pullReq)
		if err != nil {
			continue
		}

		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: normalizeArchitecture(pullReq.Architecture), Name: key}})
	}

	return reqs
}

func (r *OCIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := slog.Default()

	pullReq, err := decodeImagePullRequest(req.NamespacedName)
	if err != nil {
		logger.ErrorContext(ctx, "decoding OCI image pull request", "err", err, "request", req.String())
		return ctrl.Result{}, nil
	}

	imageRef := pullReq.ImageRef
	architecture := normalizeArchitecture(pullReq.Architecture)

	// Always resolve the remote digest so we detect tag updates.
	remoteDigest, repo, err := r.resolveRemoteDigest(ctx, imageRef, pullReq.PullSecretRef)
	if err != nil {
		logger.ErrorContext(ctx, "resolving OCI image digest", "err", err, "image", imageRef)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if we already have this exact digest cached.
	existingDigest := r.Cache.DigestForArchitecture(imageRef, architecture)
	if existingDigest == remoteDigest && r.Cache.IsCachedForArchitecture(remoteDigest, architecture) {
		return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
	}

	logger.InfoContext(ctx, "pulling OCI image", "image", imageRef, "digest", remoteDigest, "architecture", architecture)

	if err := r.pullAndUnpack(ctx, imageRef, remoteDigest, repo, architecture); err != nil {
		logger.ErrorContext(ctx, "pulling OCI image", "err", err, "image", imageRef, "architecture", architecture)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.Cache.SetDigestForArchitecture(imageRef, architecture, remoteDigest)
	logger.InfoContext(ctx, "OCI image cached", "image", imageRef, "digest", remoteDigest, "architecture", architecture)

	return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
}

// newRepository creates a remote.Repository for the given image reference,
// configuring plain HTTP for loopback and private-network registries.
func newRepository(imageRef string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}

	// Use plain HTTP for loopback and private-network registries.
	ociutil.ConfigurePlainHTTP(repo)

	return repo, nil
}

func encodeImagePullRequest(req imagePullRequest) (string, error) {
	req.Architecture = normalizeArchitecture(req.Architecture)

	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal image pull request: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeImagePullRequest(key client.ObjectKey) (imagePullRequest, error) {
	data, err := base64.RawURLEncoding.DecodeString(key.Name)
	if err != nil {
		return imagePullRequest{}, fmt.Errorf("decode request name: %w", err)
	}

	var req imagePullRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return imagePullRequest{}, fmt.Errorf("unmarshal request name: %w", err)
	}

	if req.ImageRef == "" {
		return imagePullRequest{}, fmt.Errorf("image reference is empty")
	}

	if key.Namespace != "" {
		req.Architecture = key.Namespace
	}

	req.Architecture = normalizeArchitecture(req.Architecture)

	return req, nil
}

// resolveRemoteDigest resolves the tag or digest in an image reference to its
// canonical digest by querying the remote registry.
func (r *OCIReconciler) resolveRemoteDigest(ctx context.Context, imageRef string, pullSecretRef *v1alpha3.NamespacedSecretReference) (string, *remote.Repository, error) {
	repo, err := newRepository(imageRef)
	if err != nil {
		return "", nil, err
	}

	if pullSecretRef != nil {
		if err := r.configureRepositoryAuth(ctx, repo, pullSecretRef); err != nil {
			return "", nil, err
		}
	}

	tagOrDigest := repo.Reference.Reference

	desc, err := repo.Resolve(ctx, tagOrDigest)
	if err != nil {
		return "", nil, fmt.Errorf("resolving image %q: %w", imageRef, err)
	}

	return desc.Digest.String(), repo, nil
}

func (r *OCIReconciler) configureRepositoryAuth(ctx context.Context, repo *remote.Repository, ref *v1alpha3.NamespacedSecretReference) error {
	if ref.Name == "" || ref.Namespace == "" {
		return fmt.Errorf("pull secret reference is incomplete")
	}

	var secret corev1.Secret
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &secret); err != nil {
		return fmt.Errorf("get pull secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	credential, err := credentialFromPullSecret(&secret, repo.Reference.Registry)
	if err != nil {
		return fmt.Errorf("pull secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Header:     auth.DefaultClient.Header.Clone(),
		Credential: auth.StaticCredential(repo.Reference.Registry, credential),
	}

	return nil
}

type dockerConfigJSON struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

func credentialFromPullSecret(secret *corev1.Secret, registry string) (auth.Credential, error) {
	switch secret.Type {
	case corev1.SecretTypeDockerConfigJson:
		data := secret.Data[corev1.DockerConfigJsonKey]
		if len(data) == 0 {
			return auth.EmptyCredential, fmt.Errorf("missing %s data", corev1.DockerConfigJsonKey)
		}

		var cfg dockerConfigJSON
		if err := json.Unmarshal(data, &cfg); err != nil {
			return auth.EmptyCredential, fmt.Errorf("parse %s: %w", corev1.DockerConfigJsonKey, err)
		}

		return credentialFromDockerAuths(cfg.Auths, registry)
	case corev1.SecretTypeDockercfg:
		data := secret.Data[corev1.DockerConfigKey]
		if len(data) == 0 {
			return auth.EmptyCredential, fmt.Errorf("missing %s data", corev1.DockerConfigKey)
		}

		var auths map[string]dockerAuthConfig
		if err := json.Unmarshal(data, &auths); err != nil {
			return auth.EmptyCredential, fmt.Errorf("parse %s: %w", corev1.DockerConfigKey, err)
		}

		return credentialFromDockerAuths(auths, registry)
	default:
		return auth.EmptyCredential, fmt.Errorf("unsupported secret type %q", secret.Type)
	}
}

func credentialFromDockerAuths(auths map[string]dockerAuthConfig, registry string) (auth.Credential, error) {
	if len(auths) == 0 {
		return auth.EmptyCredential, fmt.Errorf("docker config has no auth entries")
	}

	registry = normalizeRegistryHost(registry)
	for server, cfg := range auths {
		if registryHostsMatch(registry, normalizeRegistryHost(server)) {
			return credentialFromDockerAuthConfig(cfg)
		}
	}

	return auth.EmptyCredential, fmt.Errorf("docker config has no credentials for registry %q", registry)
}

func credentialFromDockerAuthConfig(cfg dockerAuthConfig) (auth.Credential, error) {
	username := cfg.Username
	password := cfg.Password

	if (username == "" || password == "") && cfg.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(cfg.Auth)
		if err != nil {
			return auth.EmptyCredential, fmt.Errorf("decode auth field: %w", err)
		}

		user, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return auth.EmptyCredential, fmt.Errorf("decode auth field: missing ':' separator")
		}

		if username == "" {
			username = user
		}

		if password == "" {
			password = pass
		}
	}

	credential := auth.Credential{
		Username:     username,
		Password:     password,
		RefreshToken: cfg.IdentityToken,
		AccessToken:  cfg.RegistryToken,
	}

	if credential == auth.EmptyCredential {
		return auth.EmptyCredential, fmt.Errorf("docker auth entry has no credentials")
	}

	return credential, nil
}

func normalizeRegistryHost(server string) string {
	server = strings.TrimSpace(server)
	server = strings.TrimPrefix(server, "http://")
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimSuffix(server, "/")

	if slash := strings.Index(server, "/"); slash >= 0 {
		server = server[:slash]
	}

	return strings.ToLower(server)
}

func registryHostsMatch(registry, candidate string) bool {
	if registry == candidate {
		return true
	}

	return isDockerHubRegistry(registry) && isDockerHubRegistry(candidate)
}

func isDockerHubRegistry(registry string) bool {
	switch registry {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	default:
		return false
	}
}

func (r *OCIReconciler) pullAndUnpack(ctx context.Context, imageRef, imageDigest string, repo *remote.Repository, architecture string) error {
	// Check if already cached (another reconcile may have beaten us).
	if r.Cache.IsCachedForArchitecture(imageDigest, architecture) {
		return nil
	}

	imageDir := r.Cache.ImageDirForArchitecture(imageDigest, architecture)

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
	if err := unpackOCILayout(ctx, layoutDir, tagOrDigest, imageDir, architecture); err != nil {
		os.RemoveAll(imageDir) //nolint:errcheck // Clean up partial unpack.
		return fmt.Errorf("unpack OCI image: %w", err)
	}

	// Verify /disk/ directory exists (kubevirt containerDisk convention).
	diskDir := r.Cache.DiskDirForArchitecture(imageDigest, architecture)
	if _, err := os.Stat(diskDir); err != nil {
		os.RemoveAll(imageDir) //nolint:errcheck // Clean up partial unpack.
		return fmt.Errorf("OCI image missing /disk directory")
	}

	return nil
}

// unpackOCILayout opens an OCI image layout at layoutDir and unpacks the
// image tagged with the given tag into destDir using umoci.
func unpackOCILayout(ctx context.Context, layoutDir, tag, destDir, architecture string) error {
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

	dp, err := selectPlatformDescriptor(architecture, descriptorPaths)
	if err != nil {
		return fmt.Errorf("select platform for tag %q: %w", tag, err)
	}

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
	ociutil.ConvertDockerMediaTypes(&manifest)

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

func selectPlatformDescriptor(architecture string, paths []casext.DescriptorPath) (casext.DescriptorPath, error) {
	architecture = normalizeArchitecture(architecture)
	want := cplatforms.Normalize(ispec.Platform{
		OS:           "linux",
		Architecture: architecture,
	})
	matcher := cplatforms.NewMatcher(want)

	var checked []string

	for _, dp := range paths {
		for _, step := range dp.Walk {
			if step.Platform == nil {
				continue
			}

			if matcher.Match(*step.Platform) {
				return dp, nil
			}

			checked = append(checked, fmt.Sprintf("%s/%s", step.Platform.OS, step.Platform.Architecture))
		}
	}

	// Direct single-platform image references usually do not include platform
	// metadata in the descriptor walk.
	if len(paths) == 1 && len(checked) == 0 {
		return paths[0], nil
	}

	return casext.DescriptorPath{}, fmt.Errorf(
		"no manifest found for platform linux/%s, available %q",
		architecture,
		strings.Join(checked, ","),
	)
}
