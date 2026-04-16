// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package net provides the "net" subcommand group for kubectl-unbounded,
// containing all unbounded-net kubectl plugin commands.
package net

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	unboundedv1alpha1 "github.com/Azure/unbounded-kube/api/net/v1alpha1"
)

// Build-time version info, set via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if Commit == "unknown" && s.Value != "" {
					if len(s.Value) > 7 {
						Commit = s.Value[:7]
					} else {
						Commit = s.Value
					}
				}
			case "vcs.time":
				if BuildTime == "unknown" && s.Value != "" {
					BuildTime = s.Value
				}
			}
		}
	}
}

const (
	defaultControllerSelector = "app.kubernetes.io/name=unbounded-net-controller"
	defaultControllerDeploy   = "unbounded-net-controller"
	defaultControllerService  = "unbounded-net-controller"
	defaultControllerPort     = "9999"
	defaultNodeSelector       = "app.kubernetes.io/name=unbounded-net-node"
	defaultNodeContainer      = "node"
)

var supportedCreateResources = map[string]schema.GroupVersionResource{
	"site": {
		Group:    unboundedv1alpha1.GroupName,
		Version:  "v1alpha1",
		Resource: "sites",
	},
	"gatewaypool": {
		Group:    unboundedv1alpha1.GroupName,
		Version:  "v1alpha1",
		Resource: "gatewaypools",
	},
	"sitepeering": {
		Group:    unboundedv1alpha1.GroupName,
		Version:  "v1alpha1",
		Resource: "sitepeerings",
	},
	"sitegatewaypoolassignment": {
		Group:    unboundedv1alpha1.GroupName,
		Version:  "v1alpha1",
		Resource: "sitegatewaypoolassignments",
	},
	"gatewaypoolpeering": {
		Group:    unboundedv1alpha1.GroupName,
		Version:  "v1alpha1",
		Resource: "gatewaypoolpeerings",
	},
}

var symlinkCreateNames = []string{
	"kubectl_complete-unbounded-net",
	"kubectl-create-site",
	"kubectl-create-st",
	"kubectl-create-gatewaypool",
	"kubectl-create-gp",
	"kubectl-create-sitepeering",
	"kubectl-create-spr",
	"kubectl-create-sitegatewaypoolassignment",
	"kubectl-create-sgpa",
	"kubectl-create-gatewaypoolpeering",
	"kubectl-create-gpp",
}

// pluginRuntime carries shared clients and config behavior for all commands.
type pluginRuntime struct {
	configFlags *genericclioptions.ConfigFlags
}

// newPluginRuntime creates the shared runtime with kubectl-compatible config flags.
func newPluginRuntime() *pluginRuntime {
	return &pluginRuntime{
		configFlags: genericclioptions.NewConfigFlags(true),
	}
}

// restConfig resolves REST config using kubeconfig/context flags.
func (p *pluginRuntime) restConfig() (*rest.Config, error) {
	return p.configFlags.ToRESTConfig()
}

// namespace resolves the namespace for unbounded-net components.
// If --namespace was explicitly provided, it is used directly.
// Otherwise, tries the current context namespace, then "unbounded-net",
// then "kube-system", returning the first that contains unbounded-net pods.
func (p *pluginRuntime) namespace() (string, error) {
	ns, overridden, err := p.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return "", err
	}

	if overridden {
		return ns, nil
	}

	client, err := p.kubeClient()
	if err != nil {
		// Can't probe the cluster; return the raw namespace.
		return ns, nil
	}

	candidates := deduplicateStrings(ns, "unbounded-net", "kube-system")

	const selector = "app.kubernetes.io/name in (unbounded-net-controller, unbounded-net-node)"
	for _, candidate := range candidates {
		pods, listErr := client.CoreV1().Pods(candidate).List(context.TODO(), v1.ListOptions{
			LabelSelector: selector,
			Limit:         1,
		})
		if listErr == nil && len(pods.Items) > 0 {
			return candidate, nil
		}
	}

	// No pods found in any candidate; return the context namespace as-is.
	return ns, nil
}

// deduplicateStrings returns the input values in order with duplicates removed.
func deduplicateStrings(values ...string) []string {
	seen := make(map[string]bool, len(values))

	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	return out
}

// dynamicClient builds a dynamic client from current kubeconfig settings.
func (p *pluginRuntime) dynamicClient() (dynamic.Interface, error) {
	cfg, err := p.restConfig()
	if err != nil {
		return nil, err
	}

	return dynamic.NewForConfig(cfg)
}

// kubeClient builds a typed kubernetes clientset.
func (p *pluginRuntime) kubeClient() (*kubernetes.Clientset, error) {
	cfg, err := p.restConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(cfg)
}

// NewNetCommand returns the "net" subcommand group for the kubectl-unbounded plugin.
// It registers all unbounded-net kubectl commands as subcommands.
func NewNetCommand() *cobra.Command {
	rt := newPluginRuntime()

	cmd := &cobra.Command{
		Use:               "net",
		Short:             "Unbounded-net networking commands",
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		SilenceErrors:     true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			cmd.SilenceUsage = true
		},
	}
	rt.configFlags.AddFlags(cmd.PersistentFlags())
	cmd.AddCommand(newCreateRootCommand(rt))
	cmd.AddCommand(newControllerRootCommand(rt))
	cmd.AddCommand(newDashboardCommand(rt))
	cmd.AddCommand(newNodeRootCommand(rt))
	cmd.AddCommand(newSymlinkRootCommand())
	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newOptionsCommand())
	cobra.AddTemplateFunc("argv0", usageArgv0)
	cobra.AddTemplateFunc("displayUseLine", displayUseLine)
	cobra.AddTemplateFunc("displayCommandPath", displayCommandPath)
	cobra.AddTemplateFunc("indent2", indent2)
	applyUsageTemplateWithoutGlobalFlags(cmd)

	// Hide inherited configFlags from completions (still functional if typed).
	// They remain visible via "kubectl unbounded net options".
	cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		f.Hidden = true
	})

	return cmd
}

const subcommandUsageTemplate = `{{with .Long}}{{. | trimTrailingWhitespaces | indent2}}

{{end}}{{if .HasAvailableSubCommands}}Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

{{end}}{{if .HasExample}}Examples:
{{.Example}}

{{end}}{{if and .Parent .HasAvailableLocalFlags}}Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}

{{end}}Usage:
  {{displayUseLine .}}
{{if .HasAvailableSubCommands}}
Use "{{displayCommandPath .}} [command] --help" for more information about a command.{{end}}
Use "{{argv0}} options" for a list of global command-line options (applies to all commands).
`

// applyUsageTemplateWithoutGlobalFlags sets a custom help/usage template that
// hides inherited (global) flags and includes Long description and Examples.
func applyUsageTemplateWithoutGlobalFlags(cmd *cobra.Command) {
	cmd.SetUsageTemplate(subcommandUsageTemplate)
	cmd.SetHelpTemplate("{{.Short}}\n\n" + subcommandUsageTemplate)

	for _, child := range cmd.Commands() {
		applyUsageTemplateWithoutGlobalFlags(child)
	}
}

// invocationPrefix overrides the command path in usage output when the binary
// is invoked via a kubectl-create-* symlink.
var invocationPrefix string

// usageArgv0 returns the command name shown in usage output.
// kubectl displays plugin names without hyphens (unbounded-net -> unbounded net).
func usageArgv0() string {
	if invocationPrefix != "" {
		return invocationPrefix
	}

	return "kubectl unbounded net"
}

// displayUseLine returns a kubectl-style use line for CLI help output.
func displayUseLine(cmd *cobra.Command) string {
	if invocationPrefix != "" {
		// For kubectl-create-* dispatch, show just the flags portion.
		use := cmd.Use
		if i := strings.IndexByte(use, ' '); i >= 0 {
			return invocationPrefix + use[i:]
		}

		return invocationPrefix
	}

	return prefixKubectlUnboundedNet(cmd.UseLine())
}

// displayCommandPath returns a kubectl-style command path for CLI help output.
func displayCommandPath(cmd *cobra.Command) string {
	if invocationPrefix != "" {
		return invocationPrefix
	}

	return prefixKubectlUnboundedNet(cmd.CommandPath())
}

// prefixKubectlUnboundedNet rewrites leading unbounded-net command names
// to "kubectl unbounded net" (kubectl displays plugin names without hyphens).
func prefixKubectlUnboundedNet(value string) string {
	trimmed := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(trimmed, "net "):
		return "kubectl unbounded net" + strings.TrimPrefix(trimmed, "net")
	case trimmed == "net":
		return "kubectl unbounded net"
	default:
		return value
	}
}

// indent2 prepends two spaces to every line in s.
func indent2(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}

	return strings.Join(lines, "\n")
}

// newVersionCommand prints build version information.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Version:    %s\nCommit:     %s\nBuild Time: %s\n", Version, Commit, BuildTime) //nolint:errcheck
		},
	}
}

// newOptionsCommand prints global flags inherited by all commands.
// It temporarily unhides flags so FlagUsages includes them.
func newOptionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "options",
		Short:  "List global command-line options",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "The following options can be passed to any command:")
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			// Temporarily unhide inherited flags for display.
			inherited := cmd.InheritedFlags()
			inherited.VisitAll(func(f *pflag.Flag) { f.Hidden = false })
			_, err = io.WriteString(cmd.OutOrStdout(), inherited.FlagUsages())
			inherited.VisitAll(func(f *pflag.Flag) { f.Hidden = true })

			return err
		},
	}
}
