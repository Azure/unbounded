// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/healthz"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/compatibility"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

// Options holds serving and delegated security settings.
type Options struct {
	SecureServing  *genericoptions.SecureServingOptions
	Authentication *genericoptions.DelegatingAuthenticationOptions
	Authorization  *genericoptions.DelegatingAuthorizationOptions
	MaxTTL         time.Duration
}

func NewOptions() *Options {
	serving := genericoptions.NewSecureServingOptions()
	serving.BindPort = 8443

	return &Options{
		SecureServing: serving, Authentication: genericoptions.NewDelegatingAuthenticationOptions(),
		Authorization: genericoptions.NewDelegatingAuthorizationOptions(), MaxTTL: time.Hour,
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	o.SecureServing.AddFlags(fs)
	o.Authentication.AddFlags(fs)
	o.Authorization.AddFlags(fs)
	fs.DurationVar(&o.MaxTTL, "max-ttl", o.MaxTTL, "Maximum and default NodeGrant lifetime.")
}

// ServerConfig supplies the ledger client and an externally managed serving certificate.
// Authenticator and Authorizer override delegation when non-nil, for embedded use and tests.
type ServerConfig struct {
	Client        kubernetes.Interface
	Namespace     string
	Cert          dynamiccertificates.CertKeyContentProvider
	Clock         clock.Clock
	Authenticator authenticator.Request
	Authorizer    authorizer.Authorizer
	// ReadyzChecks are added to the server's /readyz endpoint.
	ReadyzChecks []healthz.HealthChecker
}

type Server struct {
	server   *genericapiserver.GenericAPIServer
	informer cache.SharedIndexInformer
	handler  cache.ResourceEventHandlerRegistration
	storage  *storage
}

func (o *Options) NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Client == nil || cfg.Namespace == "" || cfg.Cert == nil {
		return nil, errors.New("client, namespace, and serving certificate are required")
	}

	if o.MaxTTL <= 0 {
		return nil, errors.New("max-ttl must be positive")
	}

	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}

	scheme := runtime.NewScheme()
	if err := rebound.AddToScheme(scheme); err != nil {
		return nil, err
	}

	scheme.AddKnownTypes(schema.GroupVersion{Group: rebound.GroupName, Version: runtime.APIVersionInternal}, &rebound.NodeGrant{}, &rebound.NodeGrantList{})
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	scheme.AddUnversionedTypes(schema.GroupVersion{Version: "v1"}, &metav1.Status{}, &metav1.APIVersions{}, &metav1.APIGroupList{}, &metav1.APIGroup{}, &metav1.APIResourceList{})

	if err := scheme.AddFieldLabelConversionFunc(rebound.SchemeGroupVersion.WithKind(rebound.Kind), func(label, value string) (string, string, error) {
		if label != "metadata.name" && label != "metadata.namespace" {
			return "", "", fmt.Errorf("unsupported field selector: %s", label)
		}

		return label, value, nil
	}); err != nil {
		return nil, err
	}

	codecs := serializer.NewCodecFactory(scheme)
	config := genericapiserver.NewConfig(codecs)
	config.BuildHandlerChainFunc = func(handler http.Handler, c *genericapiserver.Config) http.Handler {
		chain := genericapiserver.DefaultBuildHandlerChain(handler, c)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Generic handlers default legacy watches to sendInitialEvents=true.
			// Keep explicit watch-list requests distinct so legacy watches still work.
			if !r.URL.Query().Has("sendInitialEvents") {
				r = r.WithContext(context.WithValue(r.Context(), legacyWatchKey{}, true))
			}

			chain.ServeHTTP(w, r)
		})
	}
	config.EffectiveVersion = compatibility.NewEffectiveVersionFromString("1.37.0", "", "")
	config.EnableProfiling = false
	config.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(rebound.GetOpenAPIDefinitions, openapi.NewDefinitionNamer(scheme))

	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(rebound.GetOpenAPIDefinitions, openapi.NewDefinitionNamer(scheme))
	if err := o.SecureServing.ApplyTo(&config.SecureServing); err != nil {
		return nil, err
	}

	if config.SecureServing == nil {
		return nil, errors.New("secure serving must be enabled")
	}

	success := false

	defer func() {
		if !success {
			if err := config.SecureServing.Listener.Close(); err != nil {
				klog.ErrorS(err, "Close listener after server setup failure")
			}
		}
	}()

	config.SecureServing.Cert = cfg.Cert
	cert, _ := cfg.Cert.CurrentCertKeyContent()

	loopback, err := config.SecureServing.NewClientConfig(cert)
	if err != nil {
		return nil, err
	}

	config.LoopbackClientConfig = loopback

	config.ExternalAddress = config.SecureServing.Listener.Addr().String()
	if cfg.Authenticator != nil {
		config.Authentication.Authenticator = cfg.Authenticator
	} else if err := o.Authentication.ApplyTo(&config.Authentication, config.SecureServing, config.OpenAPIConfig); err != nil {
		return nil, err
	}

	if cfg.Authorizer != nil {
		config.Authorization.Authorizer = cfg.Authorizer
	} else if err := o.Authorization.ApplyTo(&config.Authorization); err != nil {
		return nil, err
	}

	config.AddReadyzChecks(cfg.ReadyzChecks...)

	server, err := config.Complete(nil).New("rebound", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	storage := newStorage(ledger.Store{Client: cfg.Client, Namespace: cfg.Namespace}, o.MaxTTL, cfg.Clock)
	group := genericapiserver.NewDefaultAPIGroupInfo(rebound.GroupName, scheme, runtime.NewParameterCodec(scheme), codecs)

	group.VersionedResourcesStorageMap[rebound.SchemeGroupVersion.Version] = map[string]rest.Storage{rebound.Resource: storage}
	if err := server.InstallAPIGroup(&group); err != nil {
		return nil, err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(cfg.Client, 0, informers.WithNamespace(cfg.Namespace), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", ledger.ConfigMapName).String()
	}))
	informer := factory.Core().V1().ConfigMaps().Informer()

	handler, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { storage.observe(obj, false) },
		UpdateFunc: func(_, obj interface{}) { storage.observe(obj, false) },
		DeleteFunc: func(obj interface{}) { storage.observe(obj, true) },
	})
	if err != nil {
		return nil, err
	}

	success = true

	return &Server{server: server, informer: informer, handler: handler, storage: storage}, nil
}

// Run starts the ledger cache before accepting API requests and blocks until shutdown.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.storage.Destroy()

	done := make(chan struct{})

	go func() { defer close(done); s.informer.RunWithContext(ctx) }()

	defer func() { cancel(); <-done }()

	if !cache.WaitForCacheSync(ctx.Done(), s.informer.HasSynced, s.handler.HasSynced) {
		if err := s.server.SecureServingInfo.Listener.Close(); err != nil {
			return err
		}

		return ctx.Err()
	}

	return s.server.PrepareRun().RunWithContext(ctx)
}
