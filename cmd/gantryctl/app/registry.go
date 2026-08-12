// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coretypedv1 "k8s.io/client-go/kubernetes/typed/core/v1"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/noderoute"
)

const (
	authAnonymous = "anonymous"
	authDelegated = "delegated"
	authBasic     = "basic"
)

func newRegistryCommand(root *rootOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "registry",
		Short: "Manage standalone Gantry registries",
	}
	command.AddCommand(newRegistryAddCommand(root))
	command.AddCommand(newRegistryListCommand(root))
	command.AddCommand(newRegistryShowCommand(root))
	command.AddCommand(newRegistryAuthCommand(root))
	command.AddCommand(newRegistryRemoveCommand(root))
	command.AddCommand(newRegistryTestCommand(root))

	return command
}

type registryAddOptions struct {
	root                 *rootOptions
	endpoint             string
	auth                 string
	username             string
	passwordStdin        bool
	fromSecret           string
	secretNamespace      string
	usernameKey          string
	passwordKey          string
	replaceExistingRoute bool
	existingOnly         bool
}

func newRegistryAddCommand(root *rootOptions) *cobra.Command {
	options := &registryAddOptions{
		root:        root,
		auth:        authDelegated,
		usernameKey: "username",
		passwordKey: "password",
	}
	command := &cobra.Command{
		Use:   "add HOST",
		Short: "Configure and route one upstream registry through Gantry",
		Example: `  gantryctl registry add myacr.azurecr.io --auth delegated
  gantryctl registry add registry.k8s.io --auth anonymous
  printf '%s' "$PASSWORD" | gantryctl registry add registry.example.com --auth basic --username reader --password-stdin
  gantryctl registry add registry.example.com --auth basic --from-secret registry-reader --secret-namespace team-a`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return options.run(command.Context(), command, args[0])
		},
	}
	command.Flags().StringVar(&options.endpoint, "endpoint", "", "Origin endpoint (defaults to https://HOST)")
	addRegistryAuthFlags(command, options)
	command.Flags().BoolVar(&options.replaceExistingRoute, "replace-existing-route", false, "Approve backing up and replacing a pre-existing hosts.toml during initial setup; later OS replacements are adopted automatically")

	return command
}

func addRegistryAuthFlags(command *cobra.Command, options *registryAddOptions) {
	command.Flags().StringVar(&options.auth, "auth", options.auth, "Authentication mode: delegated (kubelet/imagePullSecrets), anonymous, or basic (cluster-wide shared identity)")
	command.Flags().StringVar(&options.username, "username", "", "Cluster-wide shared authentication username")
	command.Flags().BoolVar(&options.passwordStdin, "password-stdin", false, "Read the cluster-wide shared authentication password from stdin")
	command.Flags().StringVar(&options.fromSecret, "from-secret", "", "Copy cluster-wide shared authentication from an existing Kubernetes Secret")
	command.Flags().StringVar(&options.secretNamespace, "secret-namespace", "", "Namespace of --from-secret (defaults to the Gantry namespace)")
	command.Flags().StringVar(&options.usernameKey, "username-key", options.usernameKey, "Username key in --from-secret")
	command.Flags().StringVar(&options.passwordKey, "password-key", options.passwordKey, "Password key in --from-secret")
}

func (o *registryAddOptions) run(ctx context.Context, command *cobra.Command, rawHost string) error {
	host, err := noderoute.NormalizeRegistryHost(rawHost)
	if err != nil {
		return err
	}

	clients, err := o.root.clusterClients()
	if err != nil {
		return err
	}

	return withRegistryMutationLock(ctx, clients.kube, o.root.namespace, o.root.timeout, func(lockCtx context.Context) error {
		return o.runLocked(lockCtx, command, clients, host)
	})
}

func (o *registryAddOptions) runLocked(ctx context.Context, command *cobra.Command, clients *clusterClients, host string) error {
	store, err := loadRegistryStore(ctx, clients.kube, o.root.namespace)
	if err != nil {
		return err
	}

	var existingUpstream *gantryconfig.UpstreamRegistry

	for index := range store.agentConfig.UpstreamRegistries {
		if store.agentConfig.UpstreamRegistries[index].Name == host {
			existingUpstream = &store.agentConfig.UpstreamRegistries[index]

			break
		}
	}

	if o.existingOnly {
		if existingUpstream == nil {
			return fmt.Errorf("registry %s is not configured", host)
		}

		if !command.Flags().Changed("auth") {
			return errors.New("--auth is required for registry auth set")
		}
	}

	endpoint := strings.TrimSpace(o.endpoint)
	if endpoint == "" && existingUpstream != nil {
		endpoint = existingUpstream.Endpoint
	} else if endpoint == "" {
		endpoint = "https://" + host
	}

	snapshot := store.snapshot()
	routeWasActive := false
	manageReplacements := false
	replaceExisting := o.replaceExistingRoute

	for _, route := range store.routes.Registries {
		if route.Host == host {
			routeWasActive = true
			manageReplacements = route.ManageReplacements
			replaceExisting = replaceExisting || route.ReplaceExisting

			break
		}
	}

	previousAuth := store.auth[host]
	selectedAuth := o.auth
	authRecord := registryAuthRecord{Mode: selectedAuth}
	authMode := selectedAuth
	credentialsPath := ""
	preserveAuth := existingUpstream != nil && !command.Flags().Changed("auth") && !hasCredentialFlags(o)

	if store.secret != nil {
		store.secret = store.secret.DeepCopy()
	}

	if preserveAuth {
		selectedAuth = previousAuth.Mode
		authRecord = previousAuth
		authMode = existingUpstream.AuthMode
		credentialsPath = existingUpstream.CredentialsPath
	} else if selectedAuth == authBasic {
		username, password, err := o.sharedCredential(ctx, command, clients.kube)
		if err != nil {
			return err
		}

		if previousAuth.CredentialKey != "" && store.secret != nil {
			delete(store.secret.Data, previousAuth.CredentialKey)
		}

		if store.secret == nil {
			store.secret = newOwnedSecret(o.root.namespace)
		}

		key := credentialKey(host)
		store.secret.Data[key] = []byte(username + ":" + password)
		authRecord.CredentialKey = key
		authMode = gantryconfig.UpstreamAuthShared
		credentialsPath = "/etc/gantry/registry/" + key
	} else if hasCredentialFlags(o) {
		return fmt.Errorf("credential flags are only valid with --auth %s", authBasic)
	} else if previousAuth.CredentialKey != "" && store.secret != nil {
		delete(store.secret.Data, previousAuth.CredentialKey)
	}

	if err := validateAuthMode(selectedAuth, endpoint); err != nil {
		return err
	}

	if store.secret != nil && len(store.secret.Data) == 0 {
		store.secret = nil
	}

	store.upsertRegistry(
		gantryconfig.UpstreamRegistry{Name: host, Endpoint: endpoint, AuthMode: authMode, CredentialsPath: credentialsPath},
		authRecord,
		noderoute.Registry{Host: host, Server: endpoint, ReplaceExisting: replaceExisting, ManageReplacements: manageReplacements},
	)

	if err := store.agentConfig.Validate(); err != nil {
		return fmt.Errorf("validate updated Gantry config: %w", err)
	}

	if err := store.routes.Validate(); err != nil {
		return fmt.Errorf("validate updated node routes: %w", err)
	}

	if routeWasActive {
		store.removeRoute(host)

		if err := applyRouteState(ctx, clients.kube, o.root.namespace, store); err != nil {
			return errors.Join(fmt.Errorf("temporarily bypass Gantry for registry update: %w", err), rollbackRouteSnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot))
		}

		if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
			return errors.Join(fmt.Errorf("wait for temporary registry bypass: %w", err), rollbackRouteSnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot))
		}

		store.upsertRegistry(
			gantryconfig.UpstreamRegistry{Name: host, Endpoint: endpoint, AuthMode: authMode, CredentialsPath: credentialsPath},
			authRecord,
			noderoute.Registry{Host: host, Server: endpoint, ReplaceExisting: replaceExisting, ManageReplacements: manageReplacements},
		)
	}

	if err := applyAgentState(ctx, clients.kube, o.root.namespace, store); err != nil {
		return errors.Join(err, rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, routeWasActive, routeWasActive))
	}

	if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, agentDaemonSetName, o.root.timeout); err != nil {
		return errors.Join(fmt.Errorf("wait for Gantry agent rollout: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, routeWasActive, routeWasActive))
	}

	image, err := installedAgentImage(ctx, clients.kube, o.root.namespace)
	if err != nil {
		return errors.Join(err, rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, routeWasActive, routeWasActive))
	}

	if err := applyNodeConfigManifest(ctx, clients.resources, o.root.namespace, image); err != nil {
		return errors.Join(err, rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, routeWasActive, routeWasActive))
	}

	if err := applyRouteState(ctx, clients.kube, o.root.namespace, store); err != nil {
		return errors.Join(fmt.Errorf("enable registry route: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, true, routeWasActive))
	}

	if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
		return errors.Join(fmt.Errorf("enable registry route: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, true, routeWasActive))
	}

	if store.manageRouteReplacements(host) {
		if err := applyRouteState(ctx, clients.kube, o.root.namespace, store); err != nil {
			return errors.Join(fmt.Errorf("enable OS replacement management: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, true, routeWasActive))
		}

		if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
			return errors.Join(fmt.Errorf("wait for OS replacement management: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot, true, routeWasActive))
		}
	}

	message := fmt.Sprintf("Registry %s configured with %s authentication.\n", host, selectedAuth)

	if selectedAuth == authBasic {
		message += "Shared authentication depends on Gantry; replacement-node pulls may retry until the node-critical agent is ready.\n"
	}

	message += fmt.Sprintf("Test it with: gantryctl registry test %s/<repository>:<tag>\n", host)

	return writeOutputf(command.OutOrStdout(), "%s", message)
}

func validateAuthMode(mode, endpoint string) error {
	switch mode {
	case authAnonymous:
		return nil
	case authDelegated, authBasic:
		if !strings.HasPrefix(strings.ToLower(endpoint), "https://") {
			return fmt.Errorf("--auth %s requires an https endpoint", mode)
		}

		return nil
	default:
		return fmt.Errorf("unsupported authentication mode %q; use anonymous, delegated, or basic", mode)
	}
}

func hasCredentialFlags(options *registryAddOptions) bool {
	return options.username != "" || options.passwordStdin || options.fromSecret != ""
}

func (o *registryAddOptions) sharedCredential(ctx context.Context, command *cobra.Command, kubeClient interface {
	CoreV1() coretypedv1.CoreV1Interface
},
) (string, string, error) {
	if o.passwordStdin == (o.fromSecret != "") {
		return "", "", errors.New("basic authentication requires exactly one of --password-stdin or --from-secret")
	}

	username := o.username
	password := ""

	if o.passwordStdin {
		data, err := io.ReadAll(io.LimitReader(command.InOrStdin(), 64*1024+1))
		if err != nil {
			return "", "", fmt.Errorf("read password from stdin: %w", err)
		}

		if len(data) > 64*1024 {
			return "", "", errors.New("password from stdin exceeds 64 KiB")
		}

		password = strings.TrimRight(string(data), "\r\n")
	} else {
		namespace := o.secretNamespace
		if namespace == "" {
			namespace = o.root.namespace
		}

		secret, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, o.fromSecret, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("read credential Secret %s/%s: %w", namespace, o.fromSecret, err)
		}

		username = string(secret.Data[o.usernameKey])
		password = string(secret.Data[o.passwordKey])
	}

	if username == "" || password == "" {
		return "", "", errors.New("basic authentication username and password must not be empty")
	}

	if strings.ContainsAny(username, ":\r\n") || strings.ContainsAny(password, "\r\n") {
		return "", "", errors.New("basic authentication username must not contain ':' and credentials must be single-line")
	}

	return username, password, nil
}

func newRegistryListCommand(root *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured registries",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			clients, err := root.clusterClients()
			if err != nil {
				return err
			}

			store, err := loadRegistryStore(command.Context(), clients.kube, root.namespace)
			if err != nil {
				return err
			}

			writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "HOST\tENDPOINT\tAUTH"); err != nil {
				return fmt.Errorf("write registry list header: %w", err)
			}

			for _, upstream := range store.agentConfig.UpstreamRegistries {
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", upstream.Name, upstream.Endpoint, store.auth[upstream.Name].Mode); err != nil {
					return fmt.Errorf("write registry list row: %w", err)
				}
			}

			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush registry list: %w", err)
			}

			return nil
		},
	}
}

func newRegistryShowCommand(root *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show HOST",
		Short: "Show one configured registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			host, err := noderoute.NormalizeRegistryHost(args[0])
			if err != nil {
				return err
			}

			clients, err := root.clusterClients()
			if err != nil {
				return err
			}

			store, err := loadRegistryStore(command.Context(), clients.kube, root.namespace)
			if err != nil {
				return err
			}

			for _, upstream := range store.agentConfig.UpstreamRegistries {
				if upstream.Name == host {
					return writeOutputf(command.OutOrStdout(), "Host: %s\nEndpoint: %s\nAuthentication: %s\n", host, upstream.Endpoint, store.auth[host].Mode)
				}
			}

			return fmt.Errorf("registry %s is not configured", host)
		},
	}
}

func newRegistryAuthCommand(root *rootOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "auth",
		Short: "Change registry authentication",
	}
	command.AddCommand(newRegistryAuthSetCommand(root))

	return command
}

func newRegistryAuthSetCommand(root *rootOptions) *cobra.Command {
	options := &registryAddOptions{
		root:         root,
		auth:         authDelegated,
		usernameKey:  "username",
		passwordKey:  "password",
		existingOnly: true,
	}
	command := &cobra.Command{
		Use:   "set HOST",
		Short: "Replace a registry's authentication mechanism",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return options.run(command.Context(), command, args[0])
		},
	}
	addRegistryAuthFlags(command, options)

	return command
}

func newRegistryRemoveCommand(root *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "remove HOST",
		Short: "Restore the node route and remove a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return removeRegistry(command.Context(), command.OutOrStdout(), root, args[0])
		},
	}
}

func removeRegistry(ctx context.Context, output io.Writer, root *rootOptions, rawHost string) error {
	host, err := noderoute.NormalizeRegistryHost(rawHost)
	if err != nil {
		return err
	}

	clients, err := root.clusterClients()
	if err != nil {
		return err
	}

	return withRegistryMutationLock(ctx, clients.kube, root.namespace, root.timeout, func(lockCtx context.Context) error {
		return removeRegistryLocked(lockCtx, output, root, clients, host)
	})
}

func removeRegistryLocked(ctx context.Context, output io.Writer, root *rootOptions, clients *clusterClients, host string) error {
	store, err := loadRegistryStore(ctx, clients.kube, root.namespace)
	if err != nil {
		return err
	}

	snapshot := store.snapshot()

	auth, found := store.removeRegistry(host)
	if !found {
		return fmt.Errorf("registry %s is not configured", host)
	}

	store.sort()

	if err := applyRouteState(ctx, clients.kube, root.namespace, store); err != nil {
		return errors.Join(err, rollbackRouteSnapshot(ctx, clients.kube, root.namespace, root.timeout, snapshot))
	}

	if err := waitForDaemonSet(ctx, clients.kube, root.namespace, nodeConfigDaemonSetName, root.timeout); err != nil {
		return errors.Join(fmt.Errorf("wait for node route restoration: %w", err), rollbackRouteSnapshot(ctx, clients.kube, root.namespace, root.timeout, snapshot))
	}

	if store.secret != nil {
		store.secret = store.secret.DeepCopy()
		if auth.CredentialKey != "" {
			delete(store.secret.Data, auth.CredentialKey)
		}

		if len(store.secret.Data) == 0 {
			store.secret = nil
		}
	}

	if err := applyAgentState(ctx, clients.kube, root.namespace, store); err != nil {
		return errors.Join(err, rollbackRegistrySnapshot(ctx, clients.kube, root.namespace, root.timeout, snapshot, true, true))
	}

	if err := waitForDaemonSet(ctx, clients.kube, root.namespace, agentDaemonSetName, root.timeout); err != nil {
		return errors.Join(fmt.Errorf("wait for Gantry agent rollout: %w", err), rollbackRegistrySnapshot(ctx, clients.kube, root.namespace, root.timeout, snapshot, true, true))
	}

	if len(store.routes.Registries) == 0 {
		if err := clients.kube.AppsV1().DaemonSets(root.namespace).Delete(ctx, nodeConfigDaemonSetName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete idle node-config DaemonSet: %w", err)
		}
	}

	return writeOutputf(output, "Registry %s removed and its node route restored.\n", host)
}
