// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package apiserver serves the rebound API from a ConfigMap ledger.
package apiserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	apistorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

type storage struct {
	store            ledger.Store
	clock            clock.Clock
	maxTTL           time.Duration
	maxNodesPerGrant int
	mu               sync.Mutex
	current          snapshot
	history          []eventBatch
	watchers         map[*grantWatch]struct{}
	closed           bool
}

func newStorage(store ledger.Store, maxTTL time.Duration, maxNodesPerGrant int, c clock.Clock) *storage {
	return &storage{
		store: store, maxTTL: maxTTL, maxNodesPerGrant: maxNodesPerGrant, clock: c,
		current: snapshot{ledger: ledger.New(), rv: "0"},
		history: []eventBatch{{rv: "0"}}, watchers: make(map[*grantWatch]struct{}),
	}
}

func (*storage) New() runtime.Object              { return &rebound.NodeGrant{} }
func (*storage) NewList() runtime.Object          { return &rebound.NodeGrantList{} }
func (*storage) NamespaceScoped() bool            { return true }
func (*storage) Kind() string                     { return rebound.Kind }
func (*storage) GetSingularName() string          { return "nodegrant" }
func (*storage) ShortNames() []string             { return []string{"ngr"} }
func (*storage) DeleteReturnsDeletedObject() bool { return true }

func (s *storage) read(ctx context.Context, rv string, match metav1.ResourceVersionMatch) (snapshot, error) {
	var requested uint64

	if rv != "" {
		var err error

		requested, err = strconv.ParseUint(rv, 10, 64)
		if err != nil {
			return snapshot{}, apierrors.NewBadRequest("resourceVersion must be an unsigned integer")
		}

		s.mu.Lock()
		current := s.current
		s.mu.Unlock()

		if match == metav1.ResourceVersionMatchExact {
			if current.rv != rv {
				return snapshot{}, apierrors.NewResourceExpired("requested resourceVersion is not available; relist")
			}

			return current, nil
		}

		newest, err := strconv.ParseUint(current.rv, 10, 64)
		if rv == "0" || (err == nil && newest >= requested) {
			return current, nil
		}
	}

	cm, err := s.store.Get(ctx)
	if err != nil {
		return snapshot{}, err
	}

	if rv != "" {
		liveRV := cm.ResourceVersion
		if liveRV == "" {
			liveRV = "0"
		}

		newest, err := strconv.ParseUint(liveRV, 10, 64)
		if err != nil {
			return snapshot{}, apierrors.NewInternalError(fmt.Errorf("invalid ledger resourceVersion: %w", err))
		}

		if newest < requested {
			return snapshot{}, apistorage.NewTooLargeResourceVersionError(requested, newest, 1)
		}
	}

	return snapshot{ledger: ledger.Parse(cm.Data), rv: cm.ResourceVersion}, nil
}

// Get, List, and the initial events of Watch return exactly what the ledger
// snapshot holds. They do not hide grants whose expiresAt has passed, because
// the clock moving does not change the ledger RV or produce a watch event.
// Filtering by the clock would let the same RV list different grants over
// time, and a watching informer would disagree with a fresh list. An expired
// grant stays visible until the reaper, or a create that reaps, removes it
// from the ledger. That write gives it a new RV and a DELETED event.
func (s *storage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	rv := ""
	if options != nil {
		rv = options.ResourceVersion
	}

	current, err := s.read(ctx, rv, metav1.ResourceVersionMatchNotOlderThan)
	if err != nil {
		return nil, err
	}

	g, ok := current.ledger.Get(types.NamespacedName{Namespace: request.NamespaceValue(ctx), Name: name})
	if !ok {
		return nil, apierrors.NewNotFound(rebound.GroupResource(), name)
	}

	return grantObject(g, current.rv), nil
}

func (s *storage) List(ctx context.Context, options *metainternal.ListOptions) (runtime.Object, error) {
	match, err := selector(ctx, options)
	if err != nil {
		return nil, err
	}

	rv := ""

	var rvMatch metav1.ResourceVersionMatch

	if options != nil {
		rv = options.ResourceVersion
		rvMatch = options.ResourceVersionMatch
	}

	current, err := s.read(ctx, rv, rvMatch)
	if err != nil {
		return nil, err
	}

	if options != nil && options.ResourceVersionMatch == metav1.ResourceVersionMatchExact && current.rv != rv {
		return nil, apierrors.NewResourceExpired("requested resourceVersion is not available; relist")
	}

	result := &rebound.NodeGrantList{ListMeta: metav1.ListMeta{ResourceVersion: current.rv}, Items: []rebound.NodeGrant{}}
	for _, g := range current.ledger.Grants() {
		obj := grantObject(g, current.rv)
		if match(obj) {
			result.Items = append(result.Items, *obj)
		}
	}

	return result, nil
}

func selector(ctx context.Context, options *metainternal.ListOptions) (func(*rebound.NodeGrant) bool, error) {
	ls, fs := labels.Everything(), fields.Everything()

	if options != nil {
		if options.LabelSelector != nil {
			ls = options.LabelSelector
		}

		if options.FieldSelector != nil {
			fs = options.FieldSelector
		}
	}

	for _, req := range fs.Requirements() {
		if req.Field != "metadata.name" && req.Field != "metadata.namespace" {
			return nil, apierrors.NewBadRequest("unsupported field selector: " + req.Field)
		}
	}

	ns := request.NamespaceValue(ctx)

	return func(g *rebound.NodeGrant) bool {
		return (ns == "" || ns == g.Namespace) && ls.Matches(labels.Set(g.Labels)) &&
			fs.Matches(fields.Set{"metadata.name": g.Name, "metadata.namespace": g.Namespace})
	}, nil
}

func (s *storage) Create(ctx context.Context, obj runtime.Object, validate rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	g, ok := obj.(*rebound.NodeGrant)
	if !ok {
		return nil, apierrors.NewBadRequest("expected NodeGrant")
	}

	g = g.DeepCopy()

	ns := request.NamespaceValue(ctx)
	if ns == "" || (g.Namespace != "" && g.Namespace != ns) {
		return nil, apierrors.NewBadRequest("namespace must match the request namespace")
	}

	g.Namespace = ns

	ttl := s.maxTTL
	if g.Spec.TTL != nil {
		ttl = g.Spec.TTL.Duration
	}

	errs := apivalidation.ValidateObjectMeta(&g.ObjectMeta, true, func(name string, _ bool) []string {
		return validation.IsDNS1123Label(name)
	}, field.NewPath("metadata"))
	if g.GenerateName != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "generateName"), "use an explicit name"))
	}

	if len(g.OwnerReferences) != 0 {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "ownerReferences"), "not supported by NodeGrant"))
	}

	if len(g.Finalizers) != 0 {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "finalizers"), "not supported by NodeGrant"))
	}

	if g.DeletionTimestamp != nil {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "deletionTimestamp"), "not supported by NodeGrant"))
	}

	if g.DeletionGracePeriodSeconds != nil {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "deletionGracePeriodSeconds"), "not supported by NodeGrant"))
	}

	selectorPath := field.NewPath("spec", "nodeSelector")

	sel, selectorErr := metav1.LabelSelectorAsSelector(g.Spec.NodeSelector)
	switch {
	case g.Spec.NodeSelector == nil:
		errs = append(errs, field.Required(selectorPath, "a node selector is required"))
	case selectorErr != nil:
		errs = append(errs, field.Invalid(selectorPath, g.Spec.NodeSelector, selectorErr.Error()))
	case sel.Empty():
		errs = append(errs, field.Invalid(selectorPath, g.Spec.NodeSelector, "must not be empty; it would select every node"))
	}

	if ttl <= 0 || ttl > s.maxTTL {
		errs = append(errs, field.Invalid(field.NewPath("spec", "ttl"), ttl.String(), "must be positive and no greater than "+s.maxTTL.String()))
	}

	if len(errs) != 0 {
		return nil, apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), g.Name, errs)
	}

	holder := ""
	if user, exists := request.UserFrom(ctx); exists {
		holder = user.GetName()
	}

	record := &ledger.Grant{
		Namespace: ns, Name: g.Name, UID: uuid.NewUUID(), Selector: g.Spec.NodeSelector.DeepCopy(),
		Holder: holder, Labels: maps.Clone(g.Labels), Annotations: maps.Clone(g.Annotations),
	}

	dry := options != nil && dryRun(options.DryRun)

	// fenced holds a cooldown error that Create returns after committing
	// the confirmation of unconfirmed releases it read.
	var fenced error

	_, rv, err := s.store.Update(ctx, dry, func(l *ledger.Ledger) error {
		fenced = nil

		namespace, err := s.store.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return &mutationError{apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), g.Name,
				field.ErrorList{field.Invalid(field.NewPath("metadata", "namespace"), ns, "namespace does not exist")})}
		}

		if err != nil {
			return &mutationError{err}
		}

		if namespace.DeletionTimestamp != nil {
			return &mutationError{apierrors.NewForbidden(rebound.GroupResource(), g.Name,
				fmt.Errorf("namespace %s is terminating", ns))}
		}

		record.NamespaceUID = namespace.UID

		l.Reap(s.clock.Now(), nil)

		record.Nodes, err = s.resolveNodes(ctx, g.Name, sel)
		if err != nil {
			return &mutationError{err}
		}

		now := s.clock.Now().UTC()
		record.CreatedAt = now
		record.ExpiresAt = now.Add(ttl)

		if validate != nil {
			if err := validate(ctx, grantObject(record, "")); err != nil {
				return &mutationError{err}
			}
		}

		// A node whose previous grant just ended may still have pod
		// deletes in flight that the old grant authorized.
		if err := l.CheckCooldown(record.Nodes, now); err != nil {
			var cooldown *ledger.CooldownError
			if dry || !errors.As(err, &cooldown) || !cooldown.Unconfirmed {
				return err
			}

			// This read shows the removals committed, so confirm them now.
			// That way a removal whose follow-up confirmation was lost
			// does not fence its nodes forever. The cooldown error says to
			// retry after a full HandoffCooldown from now.
			l.Confirm(now)

			fenced = err

			return nil
		}

		return l.Add(record)
	})
	if err == nil {
		err = fenced
	}

	if err != nil {
		return nil, storageError(g.Name, err)
	}

	return grantObject(record, rv), nil
}

// Update accepts only requests that change nothing and rejects the rest, so
// grants stay immutable. It exists because kubectl validates objects by
// looking for a PATCH operation in the OpenAPI document. Without one it falls
// back to listing CRDs, which most users cannot do. A no-op update also lets
// `kubectl apply` succeed on a grant that already matches its manifest.
//
// A no-op update is confirmed against a ledger read taken after admission, so
// it reports the state at that read, which is the same guarantee the
// Kubernetes generic registry gives for no-op updates. If the grant was
// deleted meanwhile, the update fails with NotFound. If it was replaced, or
// the ledger moved past a resourceVersion the client required, it fails with
// a Conflict so the client retries against the latest version.
func (s *storage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, _ rest.ValidateObjectFunc,
	validate rest.ValidateObjectUpdateFunc, _ bool, _ *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	obj, err := s.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}

	current, ok := obj.(*rebound.NodeGrant)
	if !ok {
		return nil, false, apierrors.NewInternalError(errors.New("expected NodeGrant"))
	}

	obj, err = objInfo.UpdatedObject(ctx, current.DeepCopy())
	if err != nil {
		return nil, false, err
	}

	updated, ok := obj.(*rebound.NodeGrant)
	if !ok {
		return nil, false, apierrors.NewBadRequest("expected NodeGrant")
	}

	if updated.ResourceVersion != "" && updated.ResourceVersion != current.ResourceVersion {
		return nil, false, apierrors.NewConflict(rebound.GroupResource(), name,
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}

	if updated.UID != "" && updated.UID != current.UID {
		return nil, false, apierrors.NewConflict(rebound.GroupResource(), name, errors.New("UID does not match"))
	}

	if errs := immutableChanges(current, updated); len(errs) != 0 {
		return nil, false, apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), name, errs)
	}

	if validate != nil {
		if err := validate(ctx, current, current); err != nil {
			return nil, false, err
		}
	}

	key := types.NamespacedName{Namespace: request.NamespaceValue(ctx), Name: name}

	var (
		record *ledger.Grant
		found  bool
	)

	_, rv, err := s.store.Update(ctx, true, func(l *ledger.Ledger) error {
		record, found = l.Get(key)
		return nil
	})
	if err != nil {
		return nil, false, storageError(name, err)
	}

	if !found {
		return nil, false, apierrors.NewNotFound(rebound.GroupResource(), name)
	}

	confirmed := grantObject(record, rv)
	if confirmed.UID != current.UID {
		return nil, false, apierrors.NewConflict(rebound.GroupResource(), name, errors.New("UID does not match"))
	}

	if updated.ResourceVersion != "" && updated.ResourceVersion != confirmed.ResourceVersion {
		return nil, false, apierrors.NewConflict(rebound.GroupResource(), name,
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}

	return confirmed, false, nil
}

// immutableChanges reports each user-set field that differs between the
// stored grant and the update. Status and server-set metadata are ignored, as
// they are on create.
func immutableChanges(current, updated *rebound.NodeGrant) field.ErrorList {
	const msg = "NodeGrant is immutable; delete it and create it again"

	var errs field.ErrorList

	meta := field.NewPath("metadata")
	if updated.Name != current.Name {
		errs = append(errs, field.Forbidden(meta.Child("name"), msg))
	}

	if updated.Namespace != "" && updated.Namespace != current.Namespace {
		errs = append(errs, field.Forbidden(meta.Child("namespace"), msg))
	}

	if !maps.Equal(updated.Labels, current.Labels) {
		errs = append(errs, field.Forbidden(meta.Child("labels"), msg))
	}

	if !maps.Equal(updated.Annotations, current.Annotations) {
		errs = append(errs, field.Forbidden(meta.Child("annotations"), msg))
	}

	if len(updated.OwnerReferences) != 0 {
		errs = append(errs, field.Forbidden(meta.Child("ownerReferences"), "not supported by NodeGrant"))
	}

	if len(updated.Finalizers) != 0 {
		errs = append(errs, field.Forbidden(meta.Child("finalizers"), "not supported by NodeGrant"))
	}

	if !apiequality.Semantic.DeepEqual(updated.Spec, current.Spec) {
		errs = append(errs, field.Forbidden(field.NewPath("spec"), msg))
	}

	return errs
}

// resolveNodes returns the sorted names of the nodes that sel matches.
//
// It runs a single node list, not one GET per node:
//   - The API server filters by the grant's label selector, so the work scales
//     with the grant size, not the cluster size.
//   - Limit is maxNodesPerGrant+1. That bounds the response and is enough to
//     tell that a grant is too large without reading the remaining nodes.
//   - Create calls this inside each ledger mutate attempt, so the node set and
//     the conflict check come from one attempt and are redone together on a
//     ledger conflict. Per-node GETs would each see a different moment.
//
// Continue is only followed when the server returns a short page; it reads
// the same list snapshot.
func (s *storage) resolveNodes(ctx context.Context, name string, sel labels.Selector) ([]string, error) {
	options := metav1.ListOptions{LabelSelector: sel.String(), Limit: int64(s.maxNodesPerGrant) + 1}

	var names []string

	path := field.NewPath("spec", "nodeSelector")
	invalid := func(err *field.Error) error {
		return apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), name, field.ErrorList{err})
	}

	for {
		nodes, err := s.store.Client.CoreV1().Nodes().List(ctx, options)
		if err != nil {
			return nil, err
		}

		for _, node := range nodes.Items {
			names = append(names, node.Name)
		}

		if len(names) > s.maxNodesPerGrant {
			return nil, invalid(field.TooMany(path, len(names), s.maxNodesPerGrant))
		}

		if nodes.Continue == "" {
			break
		}

		options.Continue = nodes.Continue
		options.Limit = int64(s.maxNodesPerGrant-len(names)) + 1
	}

	if len(names) == 0 {
		return nil, invalid(field.Invalid(path, sel.String(), "matches no nodes"))
	}

	slices.Sort(names)

	return slices.Compact(names), nil
}

func dryRun(values []string) bool { return slices.Contains(values, metav1.DryRunAll) }

func storageError(name string, err error) error {
	var (
		conflict      *ledger.ConflictError
		cooldown      *ledger.CooldownError
		exists        *ledger.ExistsError
		validationErr *mutationError
	)

	switch {
	case errors.As(err, &conflict):
		return apierrors.NewConflict(rebound.GroupResource(), name, err)
	case errors.As(err, &cooldown):
		status := apierrors.NewConflict(rebound.GroupResource(), name, err)
		status.ErrStatus.Details.RetryAfterSeconds = int32(math.Ceil(cooldown.RetryAfter.Seconds()))

		return status
	case errors.As(err, &exists):
		return apierrors.NewAlreadyExists(rebound.GroupResource(), name)
	case errors.Is(err, ledger.ErrTooLarge):
		return apierrors.NewForbidden(rebound.GroupResource(), name, errors.New("ledger full"))
	case errors.As(err, &validationErr):
		return validationErr.err
	default:
		return err
	}
}

// mutationError prevents validation conflicts from entering the store's retry loop.
type mutationError struct{ err error }

func (e *mutationError) Error() string { return e.err.Error() }

func validateDelete(ctx context.Context, g *rebound.NodeGrant, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions) error {
	if options != nil && options.Preconditions != nil {
		p := options.Preconditions
		if p.ResourceVersion != nil {
			return apierrors.NewBadRequest("resourceVersion delete preconditions are not supported; use UID")
		}

		if p.UID != nil && *p.UID != g.UID {
			return apierrors.NewConflict(rebound.GroupResource(), g.Name, errors.New("UID precondition failed"))
		}
	}

	if validate != nil {
		return validate(ctx, g)
	}

	return nil
}

func (s *storage) Delete(ctx context.Context, name string, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	var deleted *rebound.NodeGrant

	_, rv, err := s.store.Update(ctx, deleteDryRun(options), func(l *ledger.Ledger) error {
		g, ok := l.Get(types.NamespacedName{Namespace: request.NamespaceValue(ctx), Name: name})
		if !ok {
			return &mutationError{apierrors.NewNotFound(rebound.GroupResource(), name)}
		}

		deleted = grantObject(g, "")
		if err := validateDelete(ctx, deleted, validate, options); err != nil {
			return &mutationError{err}
		}

		l.Remove(g.Key(), s.clock.Now())

		return nil
	})
	if err != nil {
		return nil, false, storageError(name, err)
	}

	if !deleteDryRun(options) {
		s.confirmRemovals(ctx)
	}

	deleted.ResourceVersion = rv

	return deleted, true, nil
}

func (s *storage) DeleteCollection(ctx context.Context, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternal.ListOptions) (runtime.Object, error) {
	match, err := selector(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	result := &rebound.NodeGrantList{}

	_, rv, err := s.store.Update(ctx, deleteDryRun(options), func(l *ledger.Ledger) error {
		result.Items = []rebound.NodeGrant{}

		for _, g := range l.Grants() {
			obj := grantObject(g, "")
			if !match(obj) {
				continue
			}

			if err := validateDelete(ctx, obj, validate, options); err != nil {
				return &mutationError{err}
			}

			l.Remove(g.Key(), s.clock.Now())

			result.Items = append(result.Items, *obj)
		}

		return nil
	})
	if err != nil {
		return nil, storageError("", err)
	}

	if len(result.Items) != 0 && !deleteDryRun(options) {
		s.confirmRemovals(ctx)
	}

	result.ResourceVersion = rv
	for i := range result.Items {
		result.Items[i].ResourceVersion = rv
	}

	return result, nil
}

func deleteDryRun(options *metav1.DeleteOptions) bool {
	return options != nil && dryRun(options.DryRun)
}

// confirmRemovals confirms releases after the write that removed their grants
// returned, which starts the handoff cooldown. It is best effort: the removal
// has already succeeded, and an unconfirmed release keeps its nodes fenced
// until a later Create or reaper sweep confirms it.
func (s *storage) confirmRemovals(ctx context.Context) {
	if err := s.store.Confirm(ctx, s.clock); err != nil {
		klog.FromContext(ctx).Error(err, "Confirm node grant removal; a later create or sweep will confirm it")
	}
}

func grantObject(g *ledger.Grant, rv string) *rebound.NodeGrant {
	return &rebound.NodeGrant{
		TypeMeta: metav1.TypeMeta{APIVersion: rebound.SchemeGroupVersion.String(), Kind: rebound.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: g.Name, Namespace: g.Namespace, UID: g.UID, CreationTimestamp: metav1.NewTime(g.CreatedAt),
			ResourceVersion: rv, Labels: maps.Clone(g.Labels), Annotations: maps.Clone(g.Annotations),
		},
		Spec:   rebound.NodeGrantSpec{NodeSelector: g.Selector.DeepCopy(), TTL: &metav1.Duration{Duration: g.ExpiresAt.Sub(g.CreatedAt)}},
		Status: rebound.NodeGrantStatus{Nodes: slices.Clone(g.Nodes), Holder: g.Holder, ExpiresAt: metav1.NewTime(g.ExpiresAt)},
	}
}

func (s *storage) ConvertToTable(_ context.Context, obj, options runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{}
	if opts, ok := options.(*metav1.TableOptions); !ok || !opts.NoHeaders {
		table.ColumnDefinitions = []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Nodes", Type: "string"},
			{Name: "Holder", Type: "string"},
			{Name: "Expires", Type: "string"},
			{Name: "Age", Type: "string"},
		}
	}

	var items []rebound.NodeGrant

	switch value := obj.(type) {
	case *rebound.NodeGrant:
		items = []rebound.NodeGrant{*value}
		table.ResourceVersion = value.ResourceVersion
	case *rebound.NodeGrantList:
		items = value.Items
		table.ListMeta = value.ListMeta
	default:
		return nil, fmt.Errorf("cannot convert %T to Table", obj)
	}

	for _, g := range items {
		table.Rows = append(table.Rows, metav1.TableRow{Cells: []interface{}{
			g.Name, strings.Join(g.Status.Nodes, ","), g.Status.Holder,
			g.Status.ExpiresAt.UTC().Format(time.RFC3339), duration.HumanDuration(s.clock.Since(g.CreationTimestamp.Time)),
		}, Object: runtime.RawExtension{Object: g.DeepCopy()}})
	}

	return table, nil
}
