// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

// containerd ImageSource implementation.
//
// Wire-up:
//
// - Connects to the host's containerd over a UNIX socket (default
// /run/containerd/containerd.sock). Pinned to a single containerd
// namespace at construction (default "k8s.io" - the namespace
// kubelet uses for pod containers).
//
// - List(ctx) walks every image in the namespace and resolves each
// image's full blob set (manifest + config + layers; for image
// indexes, every per-platform manifest's blobs) from the local
// content store via images.Walk + images.Children.
//
// - Subscribe(ctx) subscribes to "/images/create" and "/images/update"
// topics and emits one ImageEvent per containerd event with the
// resolved blob set. Image-delete events are translated to
// EventDelete with the image's *current* manifest target only -
// containerd has already pruned the content store by the time the
// event fires, so we can't walk children, but the manifest digest
// is still useful for cdsub instrumentation and (future) explicit
// DHT un-Provide.
//
// Build-tag gated to linux so darwin/dev hosts compile without the
// containerd client (which only links cleanly on linux due to ttrpc +
// /proc + cgroups dependencies). Non-linux builds fall back to
// NoOpSource in cmd/gantry/.

package cdsub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
)

// DefaultContainerdSocket is the conventional path for the containerd
// gRPC API socket on a kubelet-managed node.
const DefaultContainerdSocket = "/run/containerd/containerd.sock"

// DefaultContainerdNamespace is the namespace kubelet places pod
// containers in when using containerd as its CRI runtime.
const DefaultContainerdNamespace = "k8s.io"

// ContainerdSource is a production ImageSource backed by containerd's
// events + image APIs. Construct via NewContainerdSource.
type ContainerdSource struct {
	client    *containerd.Client
	namespace string
	logger    *slog.Logger

	// connectTimeout caps how long NewContainerdSource will block when
	// dialing the socket. Defaults to 5s.
	connectTimeout time.Duration

	// mediaTypeRecorder is an optional callback invoked for every
	// descriptor visited during List/Subscribe walks. The walker
	// already has the (digest, mediaType) pair in hand - handing it
	// to containerdstore lets the transfer endpoint serve manifest
	// requests with the correct Content-Type without re-parsing
	// JSON. nil is a no-op.
	mediaTypeRecorder func(d gdigest.Digest, mediaType string)
}

// ContainerdSourceOption configures a ContainerdSource.
type ContainerdSourceOption func(*ContainerdSource)

// WithContainerdLogger plumbs a structured logger.
func WithContainerdLogger(l *slog.Logger) ContainerdSourceOption {
	return func(s *ContainerdSource) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "cdsub.containerd"))
		}
	}
}

// WithContainerdConnectTimeout overrides the dial-timeout default.
func WithContainerdConnectTimeout(d time.Duration) ContainerdSourceOption {
	return func(s *ContainerdSource) {
		if d > 0 {
			s.connectTimeout = d
		}
	}
}

// NewContainerdSource dials containerd at socket and pins all
// subsequent calls to namespace. Returns an error if the socket
// cannot be reached or the gRPC handshake fails.
func NewContainerdSource(socket, namespace string, opts ...ContainerdSourceOption) (*ContainerdSource, error) {
	if socket == "" {
		socket = DefaultContainerdSocket
	}

	if namespace == "" {
		namespace = DefaultContainerdNamespace
	}

	s := &ContainerdSource{
		namespace:      namespace,
		logger:         slog.Default().With(slog.String("subsystem", "cdsub.containerd")),
		connectTimeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}

	// containerd.New blocks until the gRPC client has a Ready connection
	// or the supplied context fires. Wrap a short timeout so an absent
	// socket doesn't stall startup forever.
	dialCtx, cancel := context.WithTimeout(context.Background(), s.connectTimeout)
	defer cancel()

	c, err := containerd.New(socket,
		containerd.WithDefaultNamespace(namespace),
		containerd.WithTimeout(s.connectTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("cdsub: dial containerd %q: %w", socket, err)
	}
	// Probe the connection so a bad socket fails fast at startup.
	if _, err := c.Version(dialCtx); err != nil {
		_ = c.Close() //nolint:errcheck // best-effort close
		return nil, fmt.Errorf("cdsub: containerd Version() probe: %w", err)
	}

	s.client = c
	s.logger.Info("connected to containerd",
		slog.String("socket", socket),
		slog.String("namespace", namespace),
	)

	return s, nil
}

// Close releases the underlying containerd gRPC client.
func (s *ContainerdSource) Close() error {
	if s == nil || s.client == nil {
		return nil
	}

	return s.client.Close()
}

// ContentStore exposes the underlying containerd content store so other
// subsystems (notably containerdstore.Store wired in storage_mode=
// containerd) can share the same gRPC connection rather than opening
// a second one. Returns nil if the source is not connected.
func (s *ContainerdSource) ContentStore() content.Store {
	if s == nil || s.client == nil {
		return nil
	}

	return s.client.ContentStore()
}

// Namespace returns the containerd namespace this source is bound to.
// Callers wiring a containerdstore.Store should use this so the two
// subsystems agree on namespace and don't diverge silently.
func (s *ContainerdSource) Namespace() string {
	if s == nil {
		return ""
	}

	return s.namespace
}

// LeasesService exposes the underlying containerd lease manager so
// containerdstore.Store can attach leases on Gantry ingest without
// opening a second gRPC connection. Returns nil if the source is
// not connected.
func (s *ContainerdSource) LeasesService() leases.Manager {
	if s == nil || s.client == nil {
		return nil
	}

	return s.client.LeasesService()
}

// SetMediaTypeRecorder registers a callback fired for every
// descriptor visited by the next List/Subscribe walk. The intent is
// to populate containerdstore.Store.RememberMediaType so the transfer
// endpoint can serve manifest GETs with the correct Content-Type
// without parsing the body. Safe to call once at startup; subsequent
// calls overwrite (the source is single-owner per process). Pass nil
// to disable. Plan "Descriptor index".
func (s *ContainerdSource) SetMediaTypeRecorder(fn func(d gdigest.Digest, mediaType string)) {
	if s == nil {
		return
	}

	s.mediaTypeRecorder = fn
}

// List walks every image in the configured namespace and resolves its
// blob set from the local content store. Implements ImageSource.
//
// The returned events are tagged EventCreate so the Subscriber's
// reconciliation loop announces them as fresh state on every reconnect.
func (s *ContainerdSource) List(ctx context.Context) ([]ImageEvent, error) {
	ctx = namespaces.WithNamespace(ctx, s.namespace)

	imgs, err := s.client.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("cdsub: ListImages: %w", err)
	}

	out := make([]ImageEvent, 0, len(imgs))
	for _, img := range imgs {
		digests, err := walkBlobsWithRecorder(ctx, s.client.ContentStore(), img.Target(), s.mediaTypeRecorder)
		if err != nil {
			// One bad image (e.g. an entry whose manifest the content
			// store lacks - possible mid-pull or after a content prune)
			// shouldn't fail the whole reconciliation pass.
			s.logger.Debug("cdsub: walk failed",
				slog.String("image", img.Name()),
				slog.Any("err", err),
			)

			continue
		}

		if len(digests) == 0 {
			continue
		}

		out = append(out, ImageEvent{
			Kind:     EventCreate,
			Registry: registryFromImage(img.Name()),
			Image:    img.Name(),
			Digests:  digests,
		})
	}

	return out, nil
}

// Subscribe streams ImageEvents for the lifetime of ctx. Implements
// ImageSource. Closes the returned channel when the underlying
// containerd subscription errors or ctx is cancelled - the Subscriber
// reconnect loop then handles backoff + reconciliation.
func (s *ContainerdSource) Subscribe(ctx context.Context) (<-chan ImageEvent, error) {
	ctx = namespaces.WithNamespace(ctx, s.namespace)

	// Filter to image-lifecycle topics plus content topics so the
	// advertiser can react to bare content eviction promptly. The
	// events API uses fnmatch-style filters; `topic~=` is the regex
	// form. Unknown events are ignored by decodeEvent's default arm,
	// so subscribing to topics that may be absent in some containerd
	// versions is safe.
	eventsCh, errCh := s.client.Subscribe(ctx,
		`topic~="/images/create"`,
		`topic~="/images/update"`,
		`topic~="/images/delete"`,
		`topic~="/content/delete"`,
	)

	out := make(chan ImageEvent, 16)

	go func() {
		defer close(out)

		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					s.logger.Debug("cdsub: subscribe stream lost", slog.Any("err", err))
				}

				return
			case env, ok := <-eventsCh:
				if !ok {
					return
				}

				if env.Event == nil {
					continue
				}

				evt, err := s.decodeEvent(ctx, env.Topic, env.Event)
				if err != nil {
					s.logger.Debug("cdsub: decode event failed",
						slog.String("topic", env.Topic),
						slog.Any("err", err),
					)

					continue
				}

				if evt == nil {
					continue
				}

				select {
				case out <- *evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// decodeEvent unmarshals a typeurl-encoded containerd event Any and
// translates it into an ImageEvent. Returns (nil, nil) for topics we
// don't act on.
func (s *ContainerdSource) decodeEvent(ctx context.Context, topic string, raw typeurl.Any) (*ImageEvent, error) {
	v, err := typeurl.UnmarshalAny(raw)
	if err != nil {
		return nil, err
	}

	switch e := v.(type) {
	case *eventstypes.ImageCreate:
		return s.buildEvent(ctx, EventCreate, e.Name)
	case *eventstypes.ImageUpdate:
		return s.buildEvent(ctx, EventUpdate, e.Name)
	case *eventstypes.ImageDelete:
		// Best-effort: containerd has already pruned the image record by
		// the time this fires; emit only the registry/name with no
		// digests so callers can correlate logs. The Subscriber's
		// announce loop tolerates an empty Digests slice.
		return &ImageEvent{
			Kind:     EventDelete,
			Registry: registryFromImage(e.Name),
			Image:    e.Name,
		}, nil
	case *eventstypes.ContentDelete:
		// Bare content deletion (e.g. ContentStore.Delete called by
		// containerd GC). The advertiser uses this to withdraw stale
		// DHT records promptly instead of waiting for the next
		// reconciliation pass.
		dgst, err := gdigest.Parse(e.Digest)
		if err != nil {
			return nil, nil //nolint:nilerr // unsupported digest algorithm; safe to skip
		}

		return &ImageEvent{
			Kind:    EventDelete,
			Digests: []gdigest.Digest{dgst},
		}, nil
	default:
		_ = topic // intentionally ignore unknown topics //nolint:errcheck // best-effort
		return nil, nil
	}
}

// buildEvent resolves the named image to its blob set and constructs
// an ImageEvent. Returns (nil, nil) if the image vanished between the
// event firing and us looking it up (a benign race).
func (s *ContainerdSource) buildEvent(ctx context.Context, kind ImageEventKind, name string) (*ImageEvent, error) {
	img, err := s.client.GetImage(ctx, name)
	if err != nil {
		// Image lookup races with delete are common; downgrade to
		// debug-level rather than treating as a hard error.
		s.logger.Debug("cdsub: get image failed",
			slog.String("image", name),
			slog.Any("err", err),
		)

		return nil, nil //nolint:nilerr // benign race
	}

	digests, err := walkBlobsWithRecorder(ctx, s.client.ContentStore(), img.Target(), s.mediaTypeRecorder)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", name, err)
	}

	if len(digests) == 0 {
		return nil, nil
	}

	return &ImageEvent{
		Kind:     kind,
		Registry: registryFromImage(name),
		Image:    name,
		Digests:  digests,
	}, nil
}

// Inventory enumerates every sha256 digest currently present in the
// configured containerd namespace by walking ContentStore.Walk. The
// returned digests are guaranteed parseable as sha256; unsupported
// algorithms are silently skipped. This is the periodic reconciliation
// source of truth used by the advertiser to reflect bare content the
// image-events stream may have missed (e.g. content GC, manual
// content.Delete, missed events during a disconnect window).
//
// Performance: uses Info-equivalent metadata only; does NOT open
// readers. Callers should bound concurrency and add jitter when
// scheduling reconciliation passes - see inventory
// performance constraints.
func (s *ContainerdSource) Inventory(ctx context.Context) ([]gdigest.Digest, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("cdsub: containerd source not initialized")
	}

	ctx = namespaces.WithNamespace(ctx, s.namespace)

	var out []gdigest.Digest

	seen := map[string]struct{}{}

	walkErr := s.client.ContentStore().Walk(ctx, func(info content.Info) error {
		dgstStr := info.Digest.String()
		if _, ok := seen[dgstStr]; ok {
			return nil
		}

		seen[dgstStr] = struct{}{}

		d, err := gdigest.Parse(dgstStr)
		if err != nil {
			return nil //nolint:nilerr // unsupported digest algorithm; skip
		}

		out = append(out, d)

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("cdsub: ContentStore.Walk: %w", walkErr)
	}

	return out, nil
}

// registryFromImage extracts the host (registry) portion of a
// containerd image name. See the implementation in image_ref.go
// (intentionally platform-agnostic so it's unit-testable on darwin).
