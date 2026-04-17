# Proxmox PXE Lab Workflow Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor `hack/cmd/proxmox-pxe-lab` into a subcommand-based Proxmox workflow helper that emits setup artifacts, renders `Machine` manifests separately, and validates readiness in workflow-friendly steps.

**Architecture:** Keep the tool intentionally Proxmox-specific, but split the current mode-driven flow into focused commands: `setup`, `render-machines`, `verify-ready`, and optionally `reset`. Centralize Proxmox fixture setup in the `setup` path, persist normalized defaults in a structured `env.yaml`, and make later commands consume those artifacts instead of re-deriving Proxmox-specific behavior.

**Tech Stack:** Go, YAML, kubectl, SSH, Proxmox, Go unit tests

---

### Task 1: Introduce subcommand parsing and command-specific config types

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/main.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config_test.go`

- [ ] **Step 1: Write the failing config tests for subcommand parsing**

Add tests to `hack/cmd/proxmox-pxe-lab/config_test.go` that lock in the new CLI shape:

```go
func TestParseCommandParsesSetupSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"setup",
		"--site", "proxmox-lab",
		"--proxmox-host", "10.10.100.2",
		"--kubeconfig", "/root/.kube/config",
		"--inventory-out", "tmp/inventory.yaml",
		"--env-out", "tmp/env.yaml",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "setup" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "setup")
	}
	if cmd.Setup == nil {
		t.Fatal("expected setup config")
	}
}

func TestParseCommandParsesRenderMachinesSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"render-machines",
		"--inventory", "tmp/inventory.yaml",
		"--env", "tmp/env.yaml",
		"--out", "tmp/machines.yaml",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "render-machines" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "render-machines")
	}
	if cmd.RenderMachines == nil {
		t.Fatal("expected render-machines config")
	}
}

func TestParseCommandRejectsUnknownSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{"proxmox-pxe-lab", "bogus"}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run the focused config tests before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestParseCommand|TestConfigValidate' -count=1
```

Expected:
- FAIL because `parseCommand` and the new command structs do not exist yet.

- [ ] **Step 3: Implement subcommand parsing and validation types**

Replace the single mode-oriented config entrypoint in `hack/cmd/proxmox-pxe-lab/config.go` with a command model like:

```go
type Command struct {
	Name           string
	Setup          *SetupConfig
	RenderMachines *RenderMachinesConfig
	VerifyReady    *VerifyReadyConfig
	Reset          *ResetConfig
}

type CommonConfig struct {
	KubeconfigPath string
	Site           string
	NodeCount      int
	RunSummaryOut  string
}

type SetupConfig struct {
	CommonConfig
	ProxmoxHost     string
	InventoryOut    string
	EnvOut          string
	PXEImage        string
	BootstrapToken  string
	BMCSecretKey    string
	StartMetalman   bool
	ProvisionFresh  bool
}

type RenderMachinesConfig struct {
	InventoryPath   string
	EnvPath         string
	OutputPath      string
	PXEImage        string
	BootstrapToken  string
	BMCSecretKey    string
}

type VerifyReadyConfig struct {
	KubeconfigPath string
	InventoryPath  string
	NodeCount      int
	RunSummaryOut  string
}

type ResetConfig struct {
	KubeconfigPath string
	ProxmoxHost    string
	InventoryPath  string
	Site           string
	NodeNames      []string
	DestroyVMs     bool
}
```

Add `parseCommand()` and subcommand-specific `Validate()` methods. Keep `sitePattern` as-is and reuse it for setup/reset where needed.

- [ ] **Step 4: Update `main.go` to dispatch through the new command model**

Change `hack/cmd/proxmox-pxe-lab/main.go` to:

```go
func main() {
	cmd, err := parseCommand()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := cmd.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := runCommand(cmd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Re-run the focused config tests after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestParseCommand|TestConfigValidate' -count=1
```

Expected:
- PASS.

- [ ] **Step 6: Commit the CLI-shape change**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/main.go hack/cmd/proxmox-pxe-lab/config.go hack/cmd/proxmox-pxe-lab/config_test.go
git commit -m "refactor: add proxmox pxe lab subcommand parsing"
```

Expected:
- Commit succeeds with only the CLI parsing and validation changes.

### Task 2: Add structured setup artifacts for normalized inventory and environment defaults

**Files:**
- Create: `hack/cmd/proxmox-pxe-lab/artifacts.go`
- Create: `hack/cmd/proxmox-pxe-lab/artifacts_test.go`
- Modify: `hack/cmd/proxmox-pxe-lab/render.go`

- [ ] **Step 1: Write the failing artifact round-trip tests**

Create `hack/cmd/proxmox-pxe-lab/artifacts_test.go` with tests for the new environment artifact and inventory helpers:

```go
func TestWriteAndReadEnvironmentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.yaml")
	want := EnvironmentFile{
		Site:               "proxmox-lab",
		ProxmoxHost:        "10.10.100.2",
		KubeconfigPath:     "/root/.kube/config",
		PXEImage:           "ghcr.io/example/custom:v1",
		BootstrapTokenName: "bootstrap-token-np1tzg",
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
			SecretKey:       "stretch-pxe-0",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	}

	if err := WriteEnvironmentFile(path, want); err != nil {
		t.Fatalf("WriteEnvironmentFile() error = %v", err)
	}
	got, err := ReadEnvironmentFile(path)
	if err != nil {
		t.Fatalf("ReadEnvironmentFile() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadEnvironmentFile() = %#v, want %#v", got, want)
	}
}

func TestReadInventoryFileUsesExistingParseInventoryNormalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	data := []byte("vms:\n  - name: stretch-pxe-4\n    vmid: 104\n    mac: \"04\"\n    intendedIPv4: \"\"\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	inv, err := ReadInventoryFile(path)
	if err != nil {
		t.Fatalf("ReadInventoryFile() error = %v", err)
	}
	if inv.VMs[0].IPv4 != "10.10.100.54" {
		t.Fatalf("inv.VMs[0].IPv4 = %q, want %q", inv.VMs[0].IPv4, "10.10.100.54")
	}
}
```

- [ ] **Step 2: Run the new artifact tests before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestWriteAndReadEnvironmentRoundTrip|TestReadInventoryFileUsesExistingParseInventoryNormalization' -count=1
```

Expected:
- FAIL because the environment artifact types and helpers do not exist yet.

- [ ] **Step 3: Implement artifact types and YAML helpers**

Create `hack/cmd/proxmox-pxe-lab/artifacts.go` with the concrete types and helpers:

```go
type EnvironmentFile struct {
	Site               string          `yaml:"site"`
	ProxmoxHost        string          `yaml:"proxmoxHost"`
	KubeconfigPath     string          `yaml:"kubeconfigPath"`
	PXEImage           string          `yaml:"pxeImage"`
	BootstrapTokenName string          `yaml:"bootstrapTokenName"`
	Redfish            RedfishDefaults `yaml:"redfish"`
	Network            NetworkDefaults `yaml:"network"`
	Artifacts          ArtifactPaths   `yaml:"artifacts,omitempty"`
}

type RedfishDefaults struct {
	URL             string `yaml:"url"`
	Username        string `yaml:"username"`
	SecretName      string `yaml:"secretName"`
	SecretNamespace string `yaml:"secretNamespace"`
	SecretKey       string `yaml:"secretKey,omitempty"`
}

type NetworkDefaults struct {
	SubnetMask string   `yaml:"subnetMask"`
	Gateway    string   `yaml:"gateway"`
	DNS        []string `yaml:"dns"`
}

type ArtifactPaths struct {
	InventoryPath       string `yaml:"inventoryPath,omitempty"`
	MachineManifestPath string `yaml:"machineManifestPath,omitempty"`
	RunSummaryPath      string `yaml:"runSummaryPath,omitempty"`
}
```

Add `WriteEnvironmentFile`, `ReadEnvironmentFile`, `WriteInventoryFile`, and `ReadInventoryFile`. Reuse `ensureParentDir` and `ParseInventory` rather than inventing a parallel normalization path.

- [ ] **Step 4: Add a rendering input helper from `EnvironmentFile`**

Add this helper to `hack/cmd/proxmox-pxe-lab/render.go`:

```go
func MachineRenderInputFromEnvironment(env EnvironmentFile, overrides RenderMachinesConfig) MachineRenderInput {
	input := MachineRenderInput{
		Site:               env.Site,
		BootstrapTokenName: env.BootstrapTokenName,
		RedfishURL:         env.Redfish.URL,
		RedfishUsername:    env.Redfish.Username,
		BMCSecretName:      env.Redfish.SecretName,
		BMCSecretNamespace: env.Redfish.SecretNamespace,
		BMCSecretKey:       env.Redfish.SecretKey,
		Image:              env.PXEImage,
		Network: MachineNetworkInput{
			SubnetMask: env.Network.SubnetMask,
			Gateway:    env.Network.Gateway,
			DNS:        env.Network.DNS,
		},
	}
	if overrides.PXEImage != "" {
		input.Image = overrides.PXEImage
	}
	if overrides.BootstrapToken != "" {
		input.BootstrapTokenName = overrides.BootstrapToken
	}
	if overrides.BMCSecretKey != "" {
		input.BMCSecretKey = overrides.BMCSecretKey
	}
	return input
}
```

This step is allowed to fail to compile until Task 3 expands `MachineRenderInput`.

- [ ] **Step 5: Re-run the artifact tests after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestWriteAndReadEnvironmentRoundTrip|TestReadInventoryFileUsesExistingParseInventoryNormalization' -count=1
```

Expected:
- PASS.

- [ ] **Step 6: Commit the artifact model**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/artifacts.go hack/cmd/proxmox-pxe-lab/artifacts_test.go hack/cmd/proxmox-pxe-lab/render.go
git commit -m "refactor: add proxmox setup artifact files"
```

Expected:
- Commit succeeds with only the artifact and helper changes.

### Task 3: Make machine rendering consume environment defaults instead of hard-coded Proxmox values

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/render.go`
- Modify: `hack/cmd/proxmox-pxe-lab/render_test.go`

- [ ] **Step 1: Write the failing rendering tests for configurable network and Redfish defaults**

Add tests to `hack/cmd/proxmox-pxe-lab/render_test.go`:

```go
func TestRenderMachinesUsesConfiguredRedfishUsernameAndSecretNamespace(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-0", VMID: 100, MAC: "02:10:10:64:00:00", IPv4: "10.10.100.50"}}}
	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-np1tzg",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "ci-user",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "lab-secrets",
		Image:              "ghcr.io/example/custom:v1",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.254.0",
			Gateway:    "10.10.100.254",
			DNS:        []string{"8.8.8.8", "1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	for _, want := range []string{
		"username: ci-user",
		"namespace: lab-secrets",
		`subnetMask: "255.255.254.0"`,
		`gateway: "10.10.100.254"`,
		`dns: ["8.8.8.8", "1.1.1.1"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestMachineRenderInputFromEnvironmentAppliesOverrides(t *testing.T) {
	env := EnvironmentFile{
		Site:               "proxmox-lab",
		PXEImage:           "ghcr.io/example/default:v1",
		BootstrapTokenName: "bootstrap-token-default",
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
			SecretKey:       "stretch-pxe-0",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	}

	input := MachineRenderInputFromEnvironment(env, RenderMachinesConfig{
		PXEImage:       "ghcr.io/example/override:v2",
		BootstrapToken: "bootstrap-token-override",
		BMCSecretKey:   "shared-key",
	})

	if input.Image != "ghcr.io/example/override:v2" {
		t.Fatalf("input.Image = %q", input.Image)
	}
	if input.BootstrapTokenName != "bootstrap-token-override" {
		t.Fatalf("input.BootstrapTokenName = %q", input.BootstrapTokenName)
	}
	if input.BMCSecretKey != "shared-key" {
		t.Fatalf("input.BMCSecretKey = %q", input.BMCSecretKey)
	}
}
```

- [ ] **Step 2: Run the focused render tests before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestRenderMachines|TestMachineRenderInputFromEnvironment' -count=1
```

Expected:
- FAIL because the render input cannot yet express those defaults.

- [ ] **Step 3: Extend the render input model and template minimally**

Update `hack/cmd/proxmox-pxe-lab/render.go` by replacing the hard-coded network and Redfish fields with explicit inputs:

```go
type MachineNetworkInput struct {
	SubnetMask string
	Gateway    string
	DNS        []string
}

type MachineRenderInput struct {
	Site                  string
	BootstrapTokenName    string
	RedfishURL            string
	RedfishUsername       string
	BMCSecretName         string
	BMCSecretNamespace    string
	BMCSecretKey          string
	Image                 string
	Network               MachineNetworkInput
	InitialRebootCounter  int
	InitialRepaveCounter  int
}
```

Update the template lines in `machineTemplate` to read from those fields:

```gotemplate
        subnetMask: {{ $.Input.Network.SubnetMask | printf "%q" }}
        gateway: {{ $.Input.Network.Gateway | printf "%q" }}
        dns: [{{ range $index, $value := $.Input.Network.DNS }}{{ if $index }}, {{ end }}{{ $value | printf "%q" }}{{ end }}]
    redfish:
      url: {{ $.Input.RedfishURL }}
      username: {{ $.Input.RedfishUsername }}
      deviceID: {{ .VMID | quoteVMID }}
      passwordRef:
        name: {{ $.Input.BMCSecretName }}
        namespace: {{ $.Input.BMCSecretNamespace }}
```

- [ ] **Step 4: Update existing tests to use the explicit defaults**

Where current tests construct `MachineRenderInput`, populate the new fields with the current Proxmox values:

```go
RedfishUsername:    "root@pam",
BMCSecretNamespace: "unbounded-kube",
Network: MachineNetworkInput{
	SubnetMask: "255.255.255.0",
	Gateway:    "10.10.100.1",
	DNS:        []string{"1.1.1.1"},
},
```

This preserves current behavior while making the inputs explicit.

- [ ] **Step 5: Re-run the focused render tests after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestRenderMachines|TestMachineRenderInputFromEnvironment' -count=1
```

Expected:
- PASS.

- [ ] **Step 6: Commit the rendering refactor**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/render.go hack/cmd/proxmox-pxe-lab/render_test.go
git commit -m "refactor: make proxmox machine rendering artifact-driven"
```

Expected:
- Commit succeeds with only the rendering input changes.

### Task 4: Implement the `setup` command to prepare the Proxmox fixture and emit artifacts

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/preflight.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run_test.go`
- Modify: `hack/cmd/proxmox-pxe-lab/preflight_test.go`

- [ ] **Step 1: Write the failing test for `setup` artifact generation**

Add a test to `hack/cmd/proxmox-pxe-lab/run_test.go` that exercises the new `setup` command without applying manifests:

```go
func TestRunSetupWritesInventoryEnvAndSummary(t *testing.T) {
	useFastWaitPolling(t)
	tempDir := t.TempDir()
	cmd := Command{
		Name: "setup",
		Setup: &SetupConfig{
			CommonConfig: CommonConfig{
				Site:           "proxmox-lab",
				NodeCount:      2,
				KubeconfigPath: "/root/.kube/config",
				RunSummaryOut:  filepath.Join(tempDir, "summary.yaml"),
			},
			ProxmoxHost:    "10.10.100.2",
			InventoryOut:   filepath.Join(tempDir, "inventory.yaml"),
			EnvOut:         filepath.Join(tempDir, "env.yaml"),
			PXEImage:       "ghcr.io/example/custom:v1",
			BootstrapToken: "bootstrap-token-np1tzg",
			BMCSecretKey:   "stretch-pxe-0",
			ProvisionFresh: true,
		},
	}

	scpCommand := "scp -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2:" + remoteInventoryPath + " " + cmd.Setup.InventoryOut
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/example/custom:v1 >/dev/null'": nil,
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['stretch-pxe-0']}": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 /root/create-stretch-pxe-vms.sh 2": nil,
		scpCommand: nil,
	}, sideEffects: map[string]func() error{
		scpCommand: func() error {
			return os.WriteFile(cmd.Setup.InventoryOut, inventoryYAML(2), 0o644)
		},
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	if _, err := os.Stat(cmd.Setup.InventoryOut); err != nil {
		t.Fatalf("inventory artifact missing: %v", err)
	}
	env, err := ReadEnvironmentFile(cmd.Setup.EnvOut)
	if err != nil {
		t.Fatalf("ReadEnvironmentFile() error = %v", err)
	}
	if env.Redfish.URL != "https://10.10.100.2:8000" {
		t.Fatalf("env.Redfish.URL = %q", env.Redfish.URL)
	}
	if env.BootstrapTokenName != "bootstrap-token-np1tzg" {
		t.Fatalf("env.BootstrapTokenName = %q", env.BootstrapTokenName)
	}
}
```

- [ ] **Step 2: Run the focused setup tests before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestRunSetup|TestPreflight' -count=1
```

Expected:
- FAIL because there is no `setup` command execution path yet.

- [ ] **Step 3: Add setup-oriented helpers to `run.go`**

Refactor `hack/cmd/proxmox-pxe-lab/run.go` around command dispatch:

```go
func runCommand(cmd Command) error {
	return runCommandWithExecutor(context.Background(), cmd, realExecutor{})
}

func runCommandWithExecutor(ctx context.Context, cmd Command, exec Executor) error {
	switch cmd.Name {
	case "setup":
		return runSetup(ctx, *cmd.Setup, exec)
	case "render-machines":
		return runRenderMachines(ctx, *cmd.RenderMachines)
	case "verify-ready":
		return runVerifyReady(ctx, *cmd.VerifyReady, exec)
	case "reset":
		return runReset(ctx, *cmd.Reset, exec)
	default:
		return fmt.Errorf("unsupported command %q", cmd.Name)
	}
}
```

Implement `runSetup` to:

- initialize a `RunSummary`
- call `RunSetupPreflight`
- optionally call `startMetalman`
- optionally provision fresh VMs
- retrieve inventory
- parse and normalize inventory
- write the normalized inventory file back to the requested path
- write `env.yaml`
- write summary output

- [ ] **Step 4: Split preflight to a setup-focused function**

In `hack/cmd/proxmox-pxe-lab/preflight.go`, add a setup-specific entrypoint that consumes `SetupConfig` directly:

```go
func RunSetupPreflight(ctx context.Context, cfg SetupConfig, exec Executor) error {
	if err := exec.Run(ctx, "kubectl", "--kubeconfig", cfg.KubeconfigPath, "cluster-info"); err != nil {
		return err
	}
	if cfg.BMCSecretKey != "" {
		if err := validateBMCSecretKeys(ctx, Config{KubeconfigPath: cfg.KubeconfigPath}, exec, []string{cfg.BMCSecretKey}); err != nil {
			return err
		}
	}
	// existing SSH / GHCR / image checks, driven from cfg.ProxmoxHost and cfg.PXEImage
	return nil
}
```

Keep `RunPreflight` only as a compatibility shim if needed during the migration, and have it delegate to the new command-specific functions.

- [ ] **Step 5: Re-run the focused setup tests after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestRunSetup|TestPreflight' -count=1
```

Expected:
- PASS.

- [ ] **Step 6: Commit the setup command**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/preflight.go hack/cmd/proxmox-pxe-lab/preflight_test.go hack/cmd/proxmox-pxe-lab/run.go hack/cmd/proxmox-pxe-lab/run_test.go
git commit -m "refactor: add proxmox pxe lab setup command"
```

Expected:
- Commit succeeds with only the setup-path changes.

### Task 5: Implement `render-machines` as a standalone workflow step

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/run.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run_test.go`

- [ ] **Step 1: Write the failing test for standalone machine rendering**

Add this test to `hack/cmd/proxmox-pxe-lab/run_test.go`:

```go
func TestRunRenderMachinesUsesInventoryAndEnvironmentArtifacts(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	envPath := filepath.Join(tempDir, "env.yaml")
	outPath := filepath.Join(tempDir, "machines.yaml")

	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := WriteEnvironmentFile(envPath, EnvironmentFile{
		Site:               "proxmox-lab",
		PXEImage:           "ghcr.io/example/default:v1",
		BootstrapTokenName: "bootstrap-token-np1tzg",
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	}); err != nil {
		t.Fatalf("WriteEnvironmentFile() error = %v", err)
	}

	cmd := Command{
		Name: "render-machines",
		RenderMachines: &RenderMachinesConfig{
			InventoryPath: invPath,
			EnvPath:       envPath,
			OutputPath:    outPath,
			PXEImage:      "ghcr.io/example/override:v2",
		},
	}

	if err := runCommandWithExecutor(context.Background(), cmd, &scriptedExecutor{}); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	manifest := string(data)
	for _, want := range []string{"kind: Machine", "image: ghcr.io/example/override:v2", "name: stretch-pxe-0", "name: stretch-pxe-1"} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("expected manifest to contain %q, got:\n%s", want, manifest)
		}
	}
}
```

- [ ] **Step 2: Run the focused render command test before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run TestRunRenderMachinesUsesInventoryAndEnvironmentArtifacts -count=1
```

Expected:
- FAIL because `runRenderMachines` does not exist yet.

- [ ] **Step 3: Implement the standalone render command**

Add `runRenderMachines` to `hack/cmd/proxmox-pxe-lab/run.go`:

```go
func runRenderMachines(_ context.Context, cfg RenderMachinesConfig) error {
	env, err := ReadEnvironmentFile(cfg.EnvPath)
	if err != nil {
		return fmt.Errorf("read environment: %w", err)
	}
	inv, err := ReadInventoryFile(cfg.InventoryPath)
	if err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}
	manifest, err := RenderMachines(inv, MachineRenderInputFromEnvironment(env, cfg))
	if err != nil {
		return fmt.Errorf("render machines: %w", err)
	}
	if err := ensureParentDir(cfg.OutputPath); err != nil {
		return fmt.Errorf("prepare machine manifest path: %w", err)
	}
	if err := os.WriteFile(cfg.OutputPath, []byte(manifest), 0o644); err != nil {
		return fmt.Errorf("write machine manifest: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Re-run the focused render command test after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run TestRunRenderMachinesUsesInventoryAndEnvironmentArtifacts -count=1
```

Expected:
- PASS.

- [ ] **Step 5: Commit the standalone render command**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/run.go hack/cmd/proxmox-pxe-lab/run_test.go
git commit -m "refactor: add standalone proxmox machine rendering command"
```

Expected:
- Commit succeeds with only the render command changes.

### Task 6: Rework `verify-ready` as a command that uses inventory artifacts only

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/run.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run_test.go`
- Read: `hack/cmd/proxmox-pxe-lab/verify.go`

- [ ] **Step 1: Write the failing verify-ready command test against the new command type**

Add this test to `hack/cmd/proxmox-pxe-lab/run_test.go`:

```go
func TestRunVerifyReadyCommandRefreshesSummaryFromExistingInventory(t *testing.T) {
	useFastWaitPolling(t)
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	summaryPath := filepath.Join(tempDir, "summary.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := Command{
		Name: "verify-ready",
		VerifyReady: &VerifyReadyConfig{
			KubeconfigPath: "/root/.kube/config",
			InventoryPath:  invPath,
			NodeCount:      2,
			RunSummaryOut:  summaryPath,
		},
	}

	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s": nil,
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s": nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{"result: success", "phase: complete", "- stretch-pxe-0", "- stretch-pxe-1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, string(data))
		}
	}
}
```

- [ ] **Step 2: Run the focused verify-ready command test before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run TestRunVerifyReadyCommandRefreshesSummaryFromExistingInventory -count=1
```

Expected:
- FAIL because the new command path is not wired yet.

- [ ] **Step 3: Implement the command-scoped verify-ready path**

Add `runVerifyReady` to `hack/cmd/proxmox-pxe-lab/run.go`:

```go
func runVerifyReady(ctx context.Context, cfg VerifyReadyConfig, exec Executor) (runErr error) {
	summary := RunSummary{Mode: "verify-ready", Result: "failure", Phase: "verify-ready"}
	defer func() {
		runErr = writeTerminalSummary(Config{RunSummaryOut: cfg.RunSummaryOut}, summary, runErr)
	}()

	inv, err := ReadInventoryFile(cfg.InventoryPath)
	if err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}
	if len(inv.VMs) != cfg.NodeCount {
		return fmt.Errorf("inventory VM count %d does not match node-count %d", len(inv.VMs), cfg.NodeCount)
	}
	summary.Nodes = inventoryNodeNames(inv)
	for _, vm := range inv.VMs {
		if err := WaitForNodeReady(ctx, exec, cfg.KubeconfigPath, vm.Name); err != nil {
			return err
		}
	}
	summary.Result = "success"
	summary.Phase = "complete"
	return nil
}
```

Do not reintroduce setup-time SSH or provisioning behavior here.

- [ ] **Step 4: Re-run the focused verify-ready command test after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run TestRunVerifyReadyCommandRefreshesSummaryFromExistingInventory -count=1
```

Expected:
- PASS.

- [ ] **Step 5: Commit the command-scoped verify-ready flow**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/run.go hack/cmd/proxmox-pxe-lab/run_test.go
git commit -m "refactor: split proxmox verify-ready command"
```

Expected:
- Commit succeeds with only the verify-ready command changes.

### Task 7: Add the `reset` command for explicit fixture cleanup

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/run.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run_test.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config_test.go`

- [ ] **Step 1: Write the failing reset tests for safe explicit cleanup**

Add these tests:

```go
func TestResetConfigRequiresHostAndAtLeastOneTarget(t *testing.T) {
	cfg := ResetConfig{KubeconfigPath: "/root/.kube/config"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRunResetDeletesMachinesNodesAndVMsFromInventory(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cmd := Command{
		Name: "reset",
		Reset: &ResetConfig{
			KubeconfigPath: "/root/.kube/config",
			ProxmoxHost:    "10.10.100.2",
			InventoryPath:  invPath,
			DestroyVMs:     true,
		},
	}
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config delete machine stretch-pxe-0 stretch-pxe-1 --ignore-not-found": nil,
		"kubectl --kubeconfig /root/.kube/config delete node stretch-pxe-0 stretch-pxe-1 --ignore-not-found": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 qm stop 100 || true; qm stop 101 || true; sleep 3; qm destroy 100 --destroy-unreferenced-disks 1 --purge 1 || true; qm destroy 101 --destroy-unreferenced-disks 1 --purge 1 || true": nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}
}
```

- [ ] **Step 2: Run the focused reset tests before implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestResetConfig|TestRunReset' -count=1
```

Expected:
- FAIL because reset validation and execution do not exist yet.

- [ ] **Step 3: Implement reset validation and execution minimally**

In `hack/cmd/proxmox-pxe-lab/config.go`, add:

```go
func (c ResetConfig) Validate() error {
	if c.KubeconfigPath == "" {
		return errors.New("kubeconfig is required")
	}
	if c.ProxmoxHost == "" {
		return errors.New("proxmox-host is required")
	}
	if c.InventoryPath == "" && len(c.NodeNames) == 0 {
		return errors.New("inventory or explicit node names are required")
	}
	return nil
}
```

In `hack/cmd/proxmox-pxe-lab/run.go`, implement `runReset` to:

- load node names and VMIDs from inventory when provided
- delete `Machine` objects
- delete `Node` objects
- optionally stop and destroy the matching VMIDs over SSH when `DestroyVMs` is true

Build the VM cleanup command from the inventory VMIDs only. Do not implement any broader site-wide cleanup.

- [ ] **Step 4: Re-run the focused reset tests after implementation**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -run 'TestResetConfig|TestRunReset' -count=1
```

Expected:
- PASS.

- [ ] **Step 5: Commit the reset command**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/run.go hack/cmd/proxmox-pxe-lab/run_test.go hack/cmd/proxmox-pxe-lab/config.go hack/cmd/proxmox-pxe-lab/config_test.go
git commit -m "refactor: add proxmox pxe lab reset command"
```

Expected:
- Commit succeeds with only the reset command changes.

### Task 8: Remove mode-centric paths and verify the new command surface end to end

**Files:**
- Modify: `hack/cmd/proxmox-pxe-lab/main.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run.go`
- Modify: `hack/cmd/proxmox-pxe-lab/config_test.go`
- Modify: `hack/cmd/proxmox-pxe-lab/run_test.go`
- Modify: `docs/superpowers/specs/2026-04-15-proxmox-pxe-lab-workflow-refactor-design.md` (only if the implementation forced a design correction)

- [ ] **Step 1: Write the failing test that the old `--mode` entrypoint is no longer the primary interface**

Add a test like:

```go
func TestParseCommandRejectsLegacyModeFlagsWithoutSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{"proxmox-pxe-lab", "--mode", "fresh"}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run the full package test suite before removing compatibility paths**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -count=1
```

Expected:
- FAIL or remain mixed until all remaining mode-based callers and tests are updated.

- [ ] **Step 3: Remove or shrink the legacy mode path**

Delete the now-obsolete mode-based validation and runner code from `config.go` and `run.go` once the subcommands fully cover the supported behavior.

The retained public surface should be:

```text
proxmox-pxe-lab setup
proxmox-pxe-lab render-machines
proxmox-pxe-lab verify-ready
proxmox-pxe-lab reset
```

If a temporary compatibility shim remains, it must be clearly marked and only delegate into the new command handlers.

- [ ] **Step 4: Run the full package test suite after the migration**

Run:

```bash
go test ./hack/cmd/proxmox-pxe-lab -count=1
```

Expected:
- PASS.

- [ ] **Step 5: Build the binary to verify the final command surface compiles**

Run:

```bash
go build -o bin/proxmox-pxe-lab ./hack/cmd/proxmox-pxe-lab
```

Expected:
- Build succeeds.

- [ ] **Step 6: Commit the final migration**

Run:

```bash
git add hack/cmd/proxmox-pxe-lab/main.go hack/cmd/proxmox-pxe-lab/config.go hack/cmd/proxmox-pxe-lab/run.go hack/cmd/proxmox-pxe-lab/config_test.go hack/cmd/proxmox-pxe-lab/run_test.go
git commit -m "refactor: convert proxmox pxe lab to workflow subcommands"
```

Expected:
- Commit succeeds with the final command-surface cleanup.

## Spec Coverage Check

- Subcommands instead of `--mode`: covered by Tasks 1 and 8.
- `setup` writes normalized inventory plus `env.yaml`: covered by Tasks 2 and 4.
- `render-machines` consumes artifacts and renders deterministically: covered by Tasks 2, 3, and 5.
- `verify-ready` validates from inventory without reprovisioning: covered by Task 6.
- `reset` is explicit and scoped: covered by Task 7.
- Proxmox-specific knowledge concentrated in setup-oriented code: covered by Tasks 3, 4, and 8.

## Self-Review Notes

- No placeholders remain: every task lists exact files, concrete tests, commands, and expected outcomes.
- The type names used in later tasks match the types introduced earlier: `Command`, `SetupConfig`, `RenderMachinesConfig`, `VerifyReadyConfig`, `ResetConfig`, and `EnvironmentFile` are used consistently.
- The plan intentionally keeps the refactor local to `hack/cmd/proxmox-pxe-lab` and does not introduce a generic backend layer, matching the approved spec.
