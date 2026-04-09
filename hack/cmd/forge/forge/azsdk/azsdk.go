// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorecloud "github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	EnvAzureTenantID       = "AZURE_TENANT_ID"
	EnvAzureClientID       = "AZURE_CLIENT_ID"
	EnvAzureAuthChainOrder = "AZURE_AUTH_CHAIN_ORDER"

	envDebugLog = "AKSIKNIFE_PKG_AZSDK_DEBUGLOG"

	ManagedIdentity  = "ManagedIdentity"
	PipelineIdentity = "PipelineIdentity"
	CLI              = "CLI"
)

type CredSource interface {
	Configure(ao AuthConfig) (azcore.TokenCredential, error)
}

func defaultChain() []string {
	return []string{
		PipelineIdentity,
		ManagedIdentity,
		CLI,
	}
}

// ChainFromEnv builds the chain by processing a config var AZURE_AUTH_CHAIN_ORDER. The
// chain is built by splitting the valid values of the var by commas. The acceptable
// credential source values are ClientSecret, ClientCertificate, ManagedIdentity, and CLI.
//
// For example, load credentials only from ManagedIdentity or CLI
// AZURE_AUTH_CHAIN_ORDER=ManagedIdentity,CLI
//
// If the chain is empty or not set then the desired chain is used.
func ChainFromEnv(desiredAuthChain ...string) []CredSource {
	chainCfg := GetAuthChainOrderConfig()
	if len(chainCfg) == 0 {
		if len(desiredAuthChain) != 0 {
			debugLog("chain override not specified with %q, using desired chain (%s)!", EnvAzureAuthChainOrder, strings.Join(desiredAuthChain, ","))
			chainCfg = desiredAuthChain
		} else {
			debugLog("chain override not specified with %q, using default (%s)!", EnvAzureAuthChainOrder, strings.Join(defaultChain(), ","))
			chainCfg = defaultChain()
		}
	}

	return parseChain(chainCfg)
}

func parseChain(chainCfg []string) []CredSource {
	var chain []CredSource

	for _, handlerName := range chainCfg {
		handlerName = strings.TrimSpace(handlerName)

		switch {
		case strings.EqualFold(handlerName, ManagedIdentity):
			debugLog("adding %q to credential source chain", ManagedIdentity)

			chain = append(chain, &ManagedIdentityCredential{})
		case strings.EqualFold(handlerName, PipelineIdentity):
			debugLog("adding %q to credential source chain", PipelineIdentity)

			chain = append(chain, &AzureDevOpsPipelineCredential{})
		case strings.EqualFold(handlerName, CLI):
			debugLog("adding %q to credential source chain", CLI)

			chain = append(chain, &CLICredential{})
		default:
			debugLog("unknown credential source in chain: %q", handlerName)
		}
	}

	return chain
}

// AuthConfig is used to configure how Azure authentication is performed in the v2 SDK.
type AuthConfig struct {
	// CloudName is the name of the Azure cloud the credential will be used
	// to communicate with. CloudName IS REQUIRED or Authenticate/Setup will
	// error. The CloudName can be either the standard Azure SDK cloud names or
	// alternate names such as the names used by the Azure CLI.
	CloudName string

	// TenantID is the unique identifier for the Azure tenant. The tenant ID
	// IS REQUIRED.
	TenantID string

	// Chain defines the chain of sources to try for authentication. An empty
	// slice will use the defaultChain.
	Chain []CredSource

	// ClientOptions are additional options that can be passed to the underlying
	// client performing authentication. Generally these do not need to be set
	// except in special circumstances.
	ClientOptions azcore.ClientOptions
}

func (cfg *AuthConfig) validateThenSetDefaults() error {
	if cfg.CloudName == "" {
		return fmt.Errorf("%T.CloudName is not set", cfg)
	}

	if cfg.TenantID == "" {
		cfg.TenantID = os.Getenv(EnvAzureTenantID)
	}

	if len(cfg.Chain) == 0 {
		debugLog("Credential chain not explicitly set, examining environment...")

		cfg.Chain = ChainFromEnv()
	}

	return nil
}

// Setup creates an authentication token and returns a basic ClientOptions
// configured for the cloud specified in AuthConfig.CloudName field.
func Setup(cfg AuthConfig) (azcore.TokenCredential, *azcore.ClientOptions, error) {
	if err := cfg.validateThenSetDefaults(); err != nil {
		return nil, nil, fmt.Errorf("validate auth config: %w", err)
	}

	// Use a custom cloud configuration. This most frequently comes up in proxy situations where
	// a custom cloud config is loaded and endpoint and token audience need to be different.
	if reflect.DeepEqual(cfg.ClientOptions.Cloud, azcorecloud.Configuration{}) {
		debugLog("custom cloud configuration not set, loading from embedded %q configuration", cfg.CloudName)

		cc, err := CloudConfig(cfg.CloudName)
		if err != nil {
			return nil, nil, fmt.Errorf("get embedded cloud configuration %q: %w", cfg.CloudName, err)
		}

		cfg.ClientOptions.Cloud = cc
	}

	var sources []azcore.TokenCredential

	for _, source := range cfg.Chain {
		tc, err := source.Configure(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("configure credential source: %w", err)
		}

		// a nil value means Configure excluded the source because certain
		// necessary prerequisites were not met, for example, the
		// client secret credential requires certain environment vars be
		// set or the values provided statically. If neither are found
		// then the source is not used.
		if tc != nil {
			debugLog("adding cred source: %T", source)

			sources = append(sources, tc)
		} else {
			debugLog("skipped cred source: %T", source)
		}
	}

	if len(sources) == 0 {
		return nil, nil, fmt.Errorf("no valid credential sources in chain")
	}

	// If there's just one valid credential source we forgo the chain because it simplifies
	// debugging. Usually there's only one source because we run in a limited amount of
	// places.
	if len(sources) == 1 {
		return sources[0], &cfg.ClientOptions, nil
	}

	cred, err := azidentity.NewChainedTokenCredential(sources, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("setup azure sdk client auth failed: %w", err)
	}

	return cred, &cfg.ClientOptions, nil
}

func CloudConfig(cloudName string) (azcorecloud.Configuration, error) {
	// TODO <plombardi89> 2026-02-3: Currently only support AzurePublicCloud.
	if cloudName != "AzurePublicCloud" {
		return azcorecloud.Configuration{}, fmt.Errorf("unsupported cloud name %q", cloudName)
	}

	return azcorecloud.AzurePublic, nil
}

func GetAuthChainOrderConfig() []string {
	chainCfg := os.Getenv(EnvAzureAuthChainOrder)
	if chainCfg == "" {
		return []string{}
	}

	return strings.Split(chainCfg, ",")
}

func isManagedIdentityResourceID(s string) bool {
	return strings.Contains(strings.ToLower(s), "/providers/microsoft.managedidentity/userassignedidentities")
}

func debugLog(msg string, args ...interface{}) {
	if v := os.Getenv(envDebugLog); v == "1" {
		msg = fmt.Sprintf(msg, args)
		log.Printf("[DEBUG] %s\n", msg)
	}
}
