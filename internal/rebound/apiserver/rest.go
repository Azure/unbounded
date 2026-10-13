// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package apiserver serves the rebound API from a ConfigMap ledger.
package apiserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

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
	"k8s.io/utils/clock"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

type storage struct {
	store    ledger.Store
	clock    clock.Clock
	maxTTL   time.Duration
	mu       sync.Mutex
	history  []snapshot
	watchers map[*grantWatch]struct{}
	closed   bool
}

func newStorage(store ledger.Store, maxTTL time.Duration, c clock.Clock) *storage {
	return &storage{
		store: store, maxTTL: maxTTL, clock: c,
		history: []snapshot{{ledger: ledger.New(), rv: "0"}}, watchers: make(map[*grantWatch]struct{}),
	}
}

func (*storage) New() runtime.Object              { return &rebound.NodeGrant{} }
func (*storage) NewList() runtime.Object          { return &rebound.NodeGrantList{} }
func (*storage) NamespaceScoped() bool            { return true }
func (*storage) Kind() string                     { return rebound.Kind }
func (*storage) GetSingularName() string          { return "nodegrant" }
func (*storage) ShortNames() []string             { return []string{"ngr"} }
func (*storage) DeleteReturnsDeletedObject() bool { return true }

func (s *storage) read(ctx context.Context, rv string) (snapshot, error) {
	if rv == "" {
		cm, err := s.store.Get(ctx)
		if err != nil {
			return snapshot{}, err
		}

		return snapshot{ledger: ledger.Parse(cm.Data), rv: cm.ResourceVersion}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.history[len(s.history)-1], nil
}

func (s *storage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	rv := ""
	if options != nil {
		rv = options.ResourceVersion
	}

	current, err := s.read(ctx, rv)
	if err != nil {
		return nil, err
	}

	g, ok := current.ledger.Get(types.NamespacedName{Namespace: request.NamespaceValue(ctx), Name: name})
	if !ok || g.Expired(s.clock.Now()) {
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
	if options != nil {
		rv = options.ResourceVersion
	}

	current, err := s.read(ctx, rv)
	if err != nil {
		return nil, err
	}

	if options != nil && options.ResourceVersionMatch == metav1.ResourceVersionMatchExact && current.rv != rv {
		return nil, apierrors.NewResourceExpired("requested resourceVersion is not available; relist")
	}

	result := &rebound.NodeGrantList{ListMeta: metav1.ListMeta{ResourceVersion: current.rv}, Items: []rebound.NodeGrant{}}
	for _, g := range current.ledger.Grants() {
		obj := grantObject(g, current.rv)
		if !g.Expired(s.clock.Now()) && match(obj) {
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

	if len(g.Spec.Nodes) == 0 {
		errs = append(errs, field.Required(field.NewPath("spec", "nodes"), "at least one node is required"))
	}

	if ttl <= 0 || ttl > s.maxTTL {
		errs = append(errs, field.Invalid(field.NewPath("spec", "ttl"), ttl.String(), "must be positive and no greater than "+s.maxTTL.String()))
	}

	seen := make(map[string]bool)

	for i, node := range g.Spec.Nodes {
		path := field.NewPath("spec", "nodes").Index(i)
		if problems := validation.IsDNS1123Subdomain(node); len(problems) != 0 {
			errs = append(errs, field.Invalid(path, node, strings.Join(problems, "; ")))
		} else if seen[node] {
			errs = append(errs, field.Duplicate(path, node))
		}

		seen[node] = true
	}

	if len(errs) != 0 {
		return nil, apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), g.Name, errs)
	}

	now := s.clock.Now().UTC()

	holder := ""
	if user, exists := request.UserFrom(ctx); exists {
		holder = user.GetName()
	}

	record := &ledger.Grant{
		Namespace: ns, Name: g.Name, UID: uuid.NewUUID(), Nodes: slices.Clone(g.Spec.Nodes),
		Holder: holder, CreatedAt: now, ExpiresAt: now.Add(ttl), Labels: maps.Clone(g.Labels), Annotations: maps.Clone(g.Annotations),
	}
	if validate != nil {
		if err := validate(ctx, grantObject(record, "")); err != nil {
			return nil, err
		}
	}

	_, rv, err := s.store.Update(ctx, options != nil && dryRun(options.DryRun), func(l *ledger.Ledger) error {
		l.Reap(s.clock.Now(), nil)

		var nodeErrors field.ErrorList

		for i, node := range record.Nodes {
			_, err := s.store.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				nodeErrors = append(nodeErrors, field.Invalid(field.NewPath("spec", "nodes").Index(i), node, "node does not exist"))
			} else if err != nil {
				return &mutationError{err}
			}
		}

		if len(nodeErrors) != 0 {
			return &mutationError{apierrors.NewInvalid(rebound.SchemeGroupVersion.WithKind(rebound.Kind).GroupKind(), g.Name, nodeErrors)}
		}

		return l.Add(record)
	})
	if err != nil {
		return nil, storageError(g.Name, err)
	}

	return grantObject(record, rv), nil
}

func dryRun(values []string) bool { return slices.Contains(values, metav1.DryRunAll) }

func storageError(name string, err error) error {
	var (
		conflict      *ledger.ConflictError
		exists        *ledger.ExistsError
		validationErr *mutationError
	)

	switch {
	case errors.As(err, &conflict):
		return apierrors.NewConflict(rebound.GroupResource(), name, err)
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

	_, rv, err := s.store.Update(ctx, options != nil && dryRun(options.DryRun), func(l *ledger.Ledger) error {
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

	deleted.ResourceVersion = rv

	return deleted, true, nil
}

func (s *storage) DeleteCollection(ctx context.Context, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternal.ListOptions) (runtime.Object, error) {
	match, err := selector(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	result := &rebound.NodeGrantList{}

	_, rv, err := s.store.Update(ctx, options != nil && dryRun(options.DryRun), func(l *ledger.Ledger) error {
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

	result.ResourceVersion = rv
	for i := range result.Items {
		result.Items[i].ResourceVersion = rv
	}

	return result, nil
}

func grantObject(g *ledger.Grant, rv string) *rebound.NodeGrant {
	return &rebound.NodeGrant{
		TypeMeta: metav1.TypeMeta{APIVersion: rebound.SchemeGroupVersion.String(), Kind: rebound.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: g.Name, Namespace: g.Namespace, UID: g.UID, CreationTimestamp: metav1.NewTime(g.CreatedAt),
			ResourceVersion: rv, Labels: maps.Clone(g.Labels), Annotations: maps.Clone(g.Annotations),
		},
		Spec:   rebound.NodeGrantSpec{Nodes: slices.Clone(g.Nodes), TTL: &metav1.Duration{Duration: g.ExpiresAt.Sub(g.CreatedAt)}},
		Status: rebound.NodeGrantStatus{Holder: g.Holder, ExpiresAt: metav1.NewTime(g.ExpiresAt)},
	}
}

func (s *storage) ConvertToTable(_ context.Context, obj runtime.Object, options runtime.Object) (*metav1.Table, error) {
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
			g.Name, strings.Join(g.Spec.Nodes, ","), g.Status.Holder,
			g.Status.ExpiresAt.UTC().Format(time.RFC3339), duration.HumanDuration(s.clock.Since(g.CreationTimestamp.Time)),
		}, Object: runtime.RawExtension{Object: g.DeepCopy()}})
	}

	return table, nil
}
