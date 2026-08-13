package node

import (
	"strings"
	"testing"
)

// TestDeviceIDBaseAcceptsAuto covers the value that asks for a derived window
// instead of a fixed floor. The literal is spelled rather than numeric because
// the number it stands for is not knowable when the environment is written: it
// depends on the node id the operator allocates later.
func TestDeviceIDBaseAcceptsAuto(t *testing.T) {
	t.Setenv(EnvNodeName, "node-a")
	t.Setenv(EnvDeviceIDBase, "AuTo")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.DeriveDeviceIDBase {
		t.Fatal("auto did not ask for a derived base")
	}

	if cfg.DeviceIDBase != 0 {
		t.Fatalf("auto also set a fixed base of %d", cfg.DeviceIDBase)
	}
}

func TestDeviceIDBaseAcceptsAFixedFloor(t *testing.T) {
	t.Setenv(EnvNodeName, "node-a")
	t.Setenv(EnvDeviceIDBase, "513")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.DeriveDeviceIDBase {
		t.Fatal("a numeric base asked for derivation")
	}

	if cfg.DeviceIDBase != 513 {
		t.Fatalf("base %d, want 513", cfg.DeviceIDBase)
	}
}

func TestDeviceIDBaseRejectsNonsense(t *testing.T) {
	for _, value := range []string{"0", "-1", "later", "1e3"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvNodeName, "node-a")
			t.Setenv(EnvDeviceIDBase, value)

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("%q was accepted", value)
			}

			if !strings.Contains(err.Error(), EnvDeviceIDBase) {
				t.Fatalf("error does not name the variable: %v", err)
			}
		})
	}
}
