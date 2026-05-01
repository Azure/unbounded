// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"fmt"
	"strings"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// ShellSingleQuote wraps v in POSIX-safe single quotes, escaping any
// embedded single quotes. The result can be used verbatim on the right-hand
// side of an `export KEY=...` statement in bash.
func ShellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// AgentInstallEnv returns the KEY=VALUE pairs that should be exported
// before the unbounded-agent install script runs, based on optional
// download overrides. Values are POSIX-single-quoted so they can be
// safely prepended to a shell command. Empty overrides are omitted.
func AgentInstallEnv(agent *v1alpha3.AgentSpec) []string {
	if agent == nil {
		return nil
	}

	var env []string

	if agent.Version != "" {
		env = append(env, fmt.Sprintf("AGENT_VERSION=%s", ShellSingleQuote(agent.Version)))
	}

	if agent.BaseURL != "" {
		env = append(env, fmt.Sprintf("AGENT_BASE_URL=%s", ShellSingleQuote(agent.BaseURL)))
	}

	if agent.URL != "" {
		env = append(env, fmt.Sprintf("AGENT_URL=%s", ShellSingleQuote(agent.URL)))
	}

	return env
}
