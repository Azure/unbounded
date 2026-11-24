// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	cplatforms "github.com/containerd/platforms"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"
	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/Azure/unbounded/internal/helpers/dockermedia"
	"github.com/Azure/unbounded/internal/helpers/ociregistry"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func init() {
	dockermedia.RegisterParsers()
}

// imageResyncInterval is how often the reconciler re-resolves remote tags
// to detect updated images pushed under the same tag.
const imageResyncInterval = 5 * time.Minute

// OCIReconciler watches Machines, retained operation media and DaemonSets for
// OCI images. Work items are deduplicated by image reference, architecture, and
// pull secret so machines sharing the same pull identity only trigger one
// download.
//
// Every replica runs this controller: each metalman serves its own cache and a
// node may be answered by any replica.
type OCIReconciler struct {
	Client                      client.Client
	Cache                       *OCICache
	Site                        string
	ProviderLabels              map[string]string
	DefaultNetbootRef           string
	DefaultNetbootPullSecretRef *v1alpha3.NamespacedSecretReference
	// BootstrapArtifactsRef is the OCI artifact that carries the offline
	// agent bootstrap bundle for the cluster's Kubernetes version. Empty
	// disables pre-warming it.
	BootstrapArtifactsRef string
}

type imagePullRequest struct {
	ImageRef      string                              `json:"imageRef"`
	Architecture  string                              `json:"architecture"`
	PullSecretRef *v1alpha3.NamespacedSecretReference `json:"pullSecretRef,omitempty"`
	// Unpack selects images whose /disk/ tree metalman serves over TFTP and
	// HTTP. Images consumed by containers or by the agent are kept as OCI
	// layouts only.
	Unpack bool `json:"unpack,omitempty"`
}

// NeedLeaderElection runs the cache warmers on every replica.
func (r *OCIReconciler) NeedLeaderElection() bool { return false }

func (r *OCIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("oci-image").
		Watches(&v1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineToImage)).
		Watches(&v1alpha3.MachineOperation{}, handler.EnqueueRequestsFromMapFunc(r.mapOperationToImage)).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(r.mapDaemonSetToMachines)).
		Complete(r)
}

// mapDaemonSetToMachines re-enqueues every Machine when a DaemonSet changes,
// so images it references are warmed for the next boot.
func (r *OCIReconciler) mapDaemonSetToMachines(ctx context.Context, _ client.Object) []reconcile.Request {
	var machines v1alpha3.MachineList
	if err := r.Client.List(ctx, &machines); err != nil {
		slog.ErrorContext(ctx, "listing machines for DaemonSet change", "err", err)
		return nil
	}

	var requests []reconcile.Request

	for i := range machines.Items {
		requests = append(requests, r.mapMachineToImage(ctx, &machines.Items[i])...)
	}

	return requests
}

// mapMachineToImage maps a Machine event to a reconcile request keyed by
// image reference. This ensures that multiple machines referencing the same
// image produce only one work item in the queue.
func (r *OCIReconciler) mapMachineToImage(ctx context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*v1alpha3.Machine)
	if !ok {
		return nil
	}

	if machine.Spec.Netboot() == nil {
		return nil
	}

	spec := machine.Spec.Netboot()
	architecture := spec.TargetArchitecture()

	refs := []imagePullRequest{{
		ImageRef:      spec.Image,
		Architecture:  architecture,
		PullSecretRef: spec.PullSecretRef,
		Unpack:        true,
	}}

	if spec.NetbootImage != "" {
		refs = append(refs, imagePullRequest{
			ImageRef:      spec.NetbootImage,
			Architecture:  architecture,
			PullSecretRef: spec.NetbootPullSecretRef,
			Unpack:        true,
		})
	} else {
		refs = append(refs, imagePullRequest{
			ImageRef:      r.DefaultNetbootRef,
			Architecture:  architecture,
			PullSecretRef: r.DefaultNetbootPullSecretRef,
			Unpack:        true,
		})
	}

	refs = append(refs, r.agentImageRequests(machine, architecture)...)
	refs = append(refs, r.bootstrapArtifactRequests(architecture)...)
	refs = append(refs, r.daemonSetRequests(ctx, machine, architecture)...)

	return imageReconcileRequests(refs)
}

// agentImageRequests returns the machine image (nspawn rootfs) images the
// agent may pull on this Machine. Without an explicit spec.agent.image the
// agent chooses between the built-in defaults on the node, so every default
// for the architecture is warmed.
func (r *OCIReconciler) agentImageRequests(machine *v1alpha3.Machine, architecture string) []imagePullRequest {
	imageRefs := []string{
		goalstates.DefaultOCIImage,
		goalstates.DefaultNvidiaOCIImage,
		goalstates.DefaultUbuntu2604OCIImage,
		goalstates.DefaultUbuntu2604NvidiaOCIImage,
		goalstates.DefaultAzureLinux3OCIImage,
		goalstates.DefaultAzureLinux3NvidiaOCIImage,
	}

	if machine.Spec.Agent != nil && machine.Spec.Agent.Image != "" {
		imageRefs = []string{machine.Spec.Agent.Image}
	}

	requests := make([]imagePullRequest, 0, len(imageRefs))
	for _, imageRef := range imageRefs {
		requests = append(requests, imagePullRequest{ImageRef: imageRef, Architecture: architecture})
	}

	return requests
}

// bootstrapArtifactRequests returns the offline bootstrap bundle, if one is
// configured. The bundle is an OCI artifact that the agent reads layer by
// layer, so metalman keeps the layout and does not unpack it.
func (r *OCIReconciler) bootstrapArtifactRequests(architecture string) []imagePullRequest {
	if r.BootstrapArtifactsRef == "" {
		return nil
	}

	return []imagePullRequest{{ImageRef: r.BootstrapArtifactsRef, Architecture: architecture}}
}

// daemonSetRequests returns the images of DaemonSets that would run on this
// Machine, so a node whose only reachable registry is metalman can start them
// after it joins.
func (r *OCIReconciler) daemonSetRequests(ctx context.Context, machine *v1alpha3.Machine, architecture string) []imagePullRequest {
	if r.Client == nil {
		return nil
	}

	var daemonSets appsv1.DaemonSetList
	if err := r.Client.List(ctx, &daemonSets); err != nil {
		slog.WarnContext(ctx, "listing DaemonSets for image warm-up", "err", err)
		return nil
	}

	labels := make(map[string]string, len(r.ProviderLabels))

	for key, value := range r.ProviderLabels {
		labels[key] = value
	}

	if machine.Spec.Kubernetes != nil {
		for key, value := range machine.Spec.Kubernetes.NodeLabels {
			labels[key] = value
		}
	}

	var requests []imagePullRequest

	for i := range daemonSets.Items {
		daemonSet := &daemonSets.Items[i]
		if !daemonSetSchedulesOn(daemonSet, labels) {
			continue
		}

		pullSecretRef := firstPullSecretRef(daemonSet.Namespace, daemonSet.Spec.Template.Spec.ImagePullSecrets)

		for _, container := range podSpecContainers(daemonSet.Spec.Template.Spec) {
			if container.Image == "" || container.ImagePullPolicy == corev1.PullNever {
				continue
			}

			requests = append(requests, imagePullRequest{
				ImageRef:      container.Image,
				Architecture:  architecture,
				PullSecretRef: pullSecretRef,
			})
		}
	}

	return requests
}

// daemonSetSchedulesOn reports whether a DaemonSet's node selector is
// satisfied by the labels the Machine's node will carry.
func daemonSetSchedulesOn(daemonSet *appsv1.DaemonSet, labels map[string]string) bool {
	for key, want := range daemonSet.Spec.Template.Spec.NodeSelector {
		if labels[key] != want {
			return false
		}
	}

	return true
}

// podSpecContainers returns the regular and init containers of a pod spec.
func podSpecContainers(spec corev1.PodSpec) []corev1.Container {
	containers := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)

	return containers
}

// firstPullSecretRef returns the first image pull secret of a workload, in the
// workload's namespace.
func firstPullSecretRef(namespace string, secrets []corev1.LocalObjectReference) *v1alpha3.NamespacedSecretReference {
	for _, secret := range secrets {
		if secret.Name == "" {
			continue
		}

		return &v1alpha3.NamespacedSecretReference{Namespace: namespace, Name: secret.Name}
	}

	return nil
}

func (r *OCIReconciler) mapOperationToImage(_ context.Context, obj client.Object) []reconcile.Request {
	op, ok := obj.(*v1alpha3.MachineOperation)
	if !ok || op.Spec.OperationKind != v1alpha3.OperationHostReplace {
		return nil
	}

	var refs []imagePullRequest

	for _, target := range op.Status.Targets {
		media := target.BootMedia
		if !mediaInSite(media, r.Site) || media.Netboot == nil {
			continue
		}

		spec := media.Netboot

		artifactsNeeded := !mediaArtifactsReleased(op, target)
		if artifactsNeeded {
			refs = append(refs, imagePullRequest{
				ImageRef:      spec.Image,
				Architecture:  spec.TargetArchitecture(),
				PullSecretRef: spec.PullSecretRef,
				Unpack:        true,
			})
		}

		if artifactsNeeded || mediaFirstBootPending(target) {
			refs = append(refs, imagePullRequest{
				ImageRef:      spec.NetbootImage,
				Architecture:  spec.TargetArchitecture(),
				PullSecretRef: spec.NetbootPullSecretRef,
				Unpack:        true,
			})
		}
	}

	return imageReconcileRequests(refs)
}

func imageReconcileRequests(refs []imagePullRequest) []reconcile.Request {
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
	if existingDigest == remoteDigest && r.isCached(pullReq, remoteDigest) {
		return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
	}

	logger.InfoContext(ctx, "pulling OCI image", "image", imageRef, "digest", remoteDigest, "architecture", architecture, "unpack", pullReq.Unpack)

	if err := r.pull(ctx, pullReq, remoteDigest, repo); err != nil {
		logger.ErrorContext(ctx, "pulling OCI image", "err", err, "image", imageRef, "architecture", architecture)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.Cache.SetDigestForArchitecture(imageRef, architecture, remoteDigest)
	logger.InfoContext(ctx, "OCI image cached", "image", imageRef, "digest", remoteDigest, "architecture", architecture)

	return ctrl.Result{RequeueAfter: imageResyncInterval}, nil
}

// isCached reports whether the content a pull request needs is already on
// disk: the unpacked /disk/ tree for the images metalman serves as files, and
// an OCI layout for the images it serves as a registry.
func (r *OCIReconciler) isCached(req imagePullRequest, digest string) bool {
	if req.Unpack {
		return r.Cache.IsCachedForArchitecture(digest, req.Architecture)
	}

	return r.Cache.HasLayout(digest)
}

// newRepository creates a remote.Repository for the given image reference,
// configuring plain HTTP for loopback and private-network registries.
func newRepository(imageRef string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}

	// Use plain HTTP for loopback and private-network registries.
	ociregistry.ConfigurePlainHTTP(repo)

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

// pull stores the image as an OCI layout under the cache directory and, when
// the request needs it, unpacks the requested platform's root filesystem.
func (r *OCIReconciler) pull(ctx context.Context, req imagePullRequest, imageDigest string, repo *remote.Repository) error {
	if r.isCached(req, imageDigest) {
		return nil
	}

	layoutDir := r.Cache.LayoutDir(imageDigest)

	if !r.Cache.HasLayout(imageDigest) {
		if err := r.copyLayout(ctx, repo, layoutDir); err != nil {
			return err
		}
	}

	if !req.Unpack {
		return nil
	}

	architecture := normalizeArchitecture(req.Architecture)
	imageDir := r.Cache.ImageDirForArchitecture(imageDigest, architecture)

	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("creating image dir: %w", err)
	}

	if err := unpackOCILayout(ctx, layoutDir, layoutTag, imageDir, architecture); err != nil {
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

// copyLayout copies an image from its registry into a local OCI layout. The
// copy lands in a partial directory first so a failed pull is never served.
func (r *OCIReconciler) copyLayout(ctx context.Context, repo *remote.Repository, layoutDir string) error {
	if err := os.MkdirAll(filepath.Dir(layoutDir), 0o755); err != nil {
		return fmt.Errorf("creating layout dir: %w", err)
	}

	partialDir := layoutDir + ".partial"

	if err := os.RemoveAll(partialDir); err != nil {
		return fmt.Errorf("clearing partial layout: %w", err)
	}

	if err := copyToLayout(ctx, repo, repo.Reference.Reference, partialDir); err != nil {
		os.RemoveAll(partialDir) //nolint:errcheck // Best effort cleanup.

		return fmt.Errorf("pull image %q: %w", repo.Reference.String(), err)
	}

	if err := os.RemoveAll(layoutDir); err != nil {
		os.RemoveAll(partialDir) //nolint:errcheck // Best effort cleanup.

		return fmt.Errorf("clearing layout: %w", err)
	}

	if err := os.Rename(partialDir, layoutDir); err != nil {
		os.RemoveAll(partialDir) //nolint:errcheck // Best effort cleanup.

		return fmt.Errorf("storing layout: %w", err)
	}

	return nil
}

// layoutTag is the tag given to an image copied into a temporary OCI layout.
// The layout index only stores names for tags, not digests. A fixed tag lets
// umoci find the image when the source reference is a digest.
const layoutTag = "metalman"

// copyToLayout copies the image named by ref from src into a new OCI layout
// at layoutDir and tags it with layoutTag.
func copyToLayout(ctx context.Context, src oras.ReadOnlyTarget, ref, layoutDir string) error {
	store, err := oci.New(layoutDir)
	if err != nil {
		return fmt.Errorf("create OCI layout store: %w", err)
	}

	if _, err := oras.Copy(ctx, src, ref, store, layoutTag, oras.DefaultCopyOptions); err != nil {
		return err
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
	dockermedia.ConvertToOCI(&manifest)

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
