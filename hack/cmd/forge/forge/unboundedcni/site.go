package unboundedcni

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
)

type SiteConfig struct {
	SiteName  string
	NodeCIDRs []string
	PodCIDRs  []string
}

func RenderSiteManifest(cfg SiteConfig) ([]byte, error) {
	t, err := Assets.ReadFile(filepath.Join("assets", "site", "site.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	return renderTemplate(t, cfg)
}

func ApplySiteManifest(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, dataDir string, cfg SiteConfig) error {
	m, err := RenderSiteManifest(cfg)
	if err != nil {
		return fmt.Errorf("error rendering site manifest: %v", err)
	}

	manifestPath := filepath.Join(dataDir, fmt.Sprintf("site-%s.yaml", cfg.SiteName))

	logger.Info("Writing site manifest", "path", manifestPath)

	if err := os.WriteFile(manifestPath, m, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %v", err)
	}

	logger.Info("Installing site", "path", manifestPath)

	if err := kube.ApplyManifests(ctx, logger, kubectl, manifestPath); err != nil {
		return fmt.Errorf("apply site manifest: %w", err)
	}

	return nil
}

type GatewayPoolConfig struct {
	PoolName  string
	AgentPool string
	Type      string
}

func RenderGatewayPoolManifest(cfg GatewayPoolConfig) ([]byte, error) {
	t, err := Assets.ReadFile(filepath.Join("assets", "site", "gatewaypool.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	return renderTemplate(t, cfg)
}

func ApplyGatewayPoolManifest(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, dataDir string, cfg GatewayPoolConfig) error {
	m, err := RenderGatewayPoolManifest(cfg)
	if err != nil {
		return fmt.Errorf("error rendering gateway pool manifest: %v", err)
	}

	manifestPath := filepath.Join(dataDir, fmt.Sprintf("gatewaypool-%s.yaml", cfg.PoolName))

	logger.Info("Writing gateway pool manifest", "path", manifestPath)

	if err := os.WriteFile(manifestPath, m, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}

	logger.Info("Installing gateway pool", "path", manifestPath)

	if err := kube.ApplyManifests(ctx, logger, kubectl, manifestPath); err != nil {
		return fmt.Errorf("apply gateway pool: %w", err)
	}

	return nil
}

type SiteGatewayPoolAssignment struct {
	SiteName        string
	SiteNames       []string
	GatewayPoolName string
}

func RenderSiteGatewayPoolAssignmentManifest(cfg SiteGatewayPoolAssignment) ([]byte, error) {
	t, err := Assets.ReadFile(filepath.Join("assets", "site", "sitegatewaypoolassignment.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	return renderTemplate(t, cfg)
}

func ApplySiteGatewayPoolAssignmentManifest(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, dataDir string, cfg SiteGatewayPoolAssignment) error {
	m, err := RenderSiteGatewayPoolAssignmentManifest(cfg)
	if err != nil {
		return fmt.Errorf("error rendering site gateway pool assignment manifest: %v", err)
	}

	manifestPath := filepath.Join(dataDir, fmt.Sprintf("gatewaypoolassignment-%s.yaml", cfg.SiteName))

	logger.Info("Writing site gateway pool assignment manifest", "path", manifestPath)

	if err := os.WriteFile(manifestPath, m, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}

	logger.Info("Installing site gateway pool assignment", "path", manifestPath)

	if err := kube.ApplyManifests(ctx, logger, kubectl, manifestPath); err != nil {
		return fmt.Errorf("apply site gateway pool assignment: %w", err)
	}

	return nil
}

func renderTemplate(templateData []byte, data interface{}) ([]byte, error) {
	tmpl, err := template.New("").Parse(string(templateData))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}

	return buf.Bytes(), nil
}
