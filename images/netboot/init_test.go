// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package netboot holds tests for the netboot installer image assets.
package netboot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	blockBegin = "# BEGIN local media functions\n"
	blockEnd   = "# END local media functions\n"
)

// localMediaBlock returns the function-only local media section of the
// installer init script.
func localMediaBlock(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile("assets/init")
	if err != nil {
		t.Fatal(err)
	}

	_, rest, ok := strings.Cut(string(content), blockBegin)
	if !ok {
		t.Fatal("init has no local media block")
	}

	block, _, ok := strings.Cut(rest, blockEnd)
	if !ok {
		t.Fatal("local media block is not terminated")
	}

	return block
}

// shells returns the POSIX shells available to run the block under. The
// Ubuntu netboot initrd runs busybox ash, so busybox is preferred.
func shells(t *testing.T) [][]string {
	t.Helper()

	var out [][]string

	if _, err := exec.LookPath("busybox"); err == nil {
		out = append(out, []string{"busybox", "sh"})
	}

	if _, err := exec.LookPath("dash"); err == nil {
		out = append(out, []string{"dash"})
	}

	if len(out) == 0 {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("no POSIX shell available")
		}

		out = append(out, []string{"sh"})
	}

	return out
}

type fixture struct {
	dir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{dir: t.TempDir()}
	for _, d := range []string{"net", "media/networkd", "bonding", "root"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return f
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()

	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// netdev creates a fake /sys/class/net entry.
func (f *fixture) netdev(t *testing.T, name, mac string, physical bool) {
	t.Helper()
	f.write(t, "net/"+name+"/address", mac+"\n")

	if physical {
		if err := os.MkdirAll(filepath.Join(f.dir, "net", name, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// run executes script after the local media block with fixture paths and a
// recording ip stub. It returns combined output and the error.
func (f *fixture) run(t *testing.T, shell []string, script string) (string, error) {
	t.Helper()

	prelude := "set -u\n" +
		"F='" + f.dir + "'\n" +
		"UB_NET=$F/net\nUB_MEDIA=$F/media\nUB_WORK=$F/work\nUB_PROC_BOND=$F/bonding\nUB_RESOLV=$F/resolv.conf\n" +
		"UB_MAP_TRIES=2\nUB_LINK_WAIT=3\nUB_FETCH_ATTEMPTS=3\n" +
		"log() { echo \"log: $*\"; }\nsleep() { :; }\n" +
		"ip_stub() { echo \"$*\" >> \"$F/ip.log\"; }\nUB_IP=ip_stub\n"

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	args := append(append([]string{}, shell[1:]...), "-c", prelude+localMediaBlock(t)+script)
	cmd := exec.CommandContext(ctx, shell[0], args...)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

func (f *fixture) read(t *testing.T, name string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

const bondNetwork = `# host network
ethernet enp1s0f0 02:00:00:00:00:01 9000
ethernet enp1s0f1 02:00:00:00:00:02 9000
bond bond0 802.3ad 100 layer3+4 fast 9000 enp1s0f0,enp1s0f1
vlan bond0.64 bond0 64 1500
address bond0 192.0.2.10/24
address bond0.64 198.51.100.10/24
route bond0 default 192.0.2.1
route bond0.64 203.0.113.0/24 198.51.100.1
dns 192.0.2.53
source bond0 192.0.2.10
`

func TestLocalMediaApplyBondVLAN(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(shell[0], func(t *testing.T) {
			f := newFixture(t)
			f.write(t, "media/network", bondNetwork)
			// Kernel names differ from declared names and MACs are uppercase.
			f.netdev(t, "eth0", "02:00:00:00:00:02", true)
			f.netdev(t, "eth1", "02:00:00:00:00:01", true)
			f.netdev(t, "lo", "00:00:00:00:00:00", false)

			out, err := f.run(t, shell, `
parse_network_file "$UB_MEDIA/network" || exit 1
apply_network || exit 1
[ "$(net_dev enp1s0f0)" = eth1 ] || exit 2
[ "$(net_dev bond0)" = bond0 ] || exit 3
[ "$(net_source_bond bond0)" = bond0 ] || exit 4
[ "$(net_source_bond bond0.64)" = bond0 ] || exit 5
`)
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}

			want := `link set dev eth1 mtu 9000
link set dev eth0 mtu 9000
link add name bond0 type bond mode 802.3ad miimon 100 xmit_hash_policy layer3+4 lacp_rate fast
link set dev eth1 down
link set dev eth1 master bond0
link set dev eth0 down
link set dev eth0 master bond0
link set dev bond0 mtu 9000
link add link bond0 name bond0.64 type vlan id 64
link set dev bond0.64 mtu 1500
link set dev eth1 up
link set dev eth0 up
link set dev bond0 up
link set dev bond0.64 up
addr replace 192.0.2.10/24 dev bond0
addr replace 198.51.100.10/24 dev bond0.64
route replace default via 192.0.2.1 dev bond0
route replace 203.0.113.0/24 via 198.51.100.1 dev bond0.64
`
			if got := f.read(t, "ip.log"); got != want {
				t.Fatalf("ip calls:\n%s\nwant:\n%s", got, want)
			}

			if got := f.read(t, "resolv.conf"); got != "nameserver 192.0.2.53\n" {
				t.Fatalf("resolv.conf = %q", got)
			}
		})
	}
}

func TestLocalMediaParseRejects(t *testing.T) {
	base := "ethernet e0 02:00:00:00:00:01 -\naddress e0 192.0.2.10/24\nsource e0 192.0.2.10\n"
	cases := map[string]string{
		"unknown directive":    base + "bridge br0 e0\n",
		"bad token":            base + "dns 192.0.2.53;reboot\n",
		"shell expansion":      base + "dns $(reboot)\n",
		"uppercase mac":        "ethernet e0 02:00:00:00:00:0A -\naddress e0 192.0.2.10/24\nsource e0 192.0.2.10\n",
		"duplicate mac":        base + "ethernet e1 02:00:00:00:00:01 -\n",
		"duplicate name":       base + "vlan e0 e0 5 -\n",
		"long name":            base + "vlan e0.this-is-too-long e0 5 -\n",
		"vlan id range":        base + "vlan e0.5000 e0 5000 -\n",
		"vlan unknown link":    base + "vlan e9.5 e9 5 -\n",
		"bond unknown member":  base + "bond bond0 802.3ad 100 - - - e0,e9\n",
		"bond bad mode":        base + "bond bond0 balance-rr 100 - - - e0\n",
		"bond shared member":   base + "bond b0 802.3ad 100 - - - e0\nbond b1 802.3ad 100 - - - e0\n",
		"bad address":          base + "address e0 192.0.2.300/24\n",
		"address unknown":      base + "address e9 192.0.2.11/24\n",
		"bad mtu":              base + "vlan e0.5 e0 5 12\n",
		"missing source":       "ethernet e0 02:00:00:00:00:01 -\n",
		"source not declared":  "ethernet e0 02:00:00:00:00:01 -\nsource e0 192.0.2.10\n",
		"too many arguments":   base + "dns 192.0.2.53 192.0.2.54\n",
		"duplicate source":     base + "source e0 192.0.2.10\n",
		"route bad next hop":   base + "route e0 default gateway\n",
		"no ethernet declared": "address e0 192.0.2.10/24\nsource e0 192.0.2.10\n",
	}

	for _, shell := range shells(t) {
		for name, network := range cases {
			t.Run(shell[0]+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				f.write(t, "media/network", network)

				out, err := f.run(t, shell, "if parse_network_file \"$UB_MEDIA/network\"; then exit 0; fi\nexit 7\n")
				if err == nil {
					t.Fatalf("accepted invalid network: %s", out)
				}

				if _, ok := err.(*exec.ExitError); !ok || !strings.Contains(err.Error(), "7") {
					t.Fatalf("unexpected error %v: %s", err, out)
				}
			})
		}
	}
}

func TestLocalMediaMapEthernetFailures(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(shell[0], func(t *testing.T) {
			f := newFixture(t)
			f.write(t, "media/network", "ethernet e0 02:00:00:00:00:01 -\naddress e0 192.0.2.10/24\nsource e0 192.0.2.10\n")

			out, err := f.run(t, shell, "parse_network_file \"$UB_MEDIA/network\" || exit 1\nif apply_network; then exit 0; fi\nexit 7\n")
			if err == nil || !strings.Contains(out, "no network device has MAC") {
				t.Fatalf("missing device: %v: %s", err, out)
			}

			f.netdev(t, "eth0", "02:00:00:00:00:01", true)
			f.netdev(t, "eth1", "02:00:00:00:00:01", true)

			out, err = f.run(t, shell, "parse_network_file \"$UB_MEDIA/network\" || exit 1\nif apply_network; then exit 0; fi\nexit 7\n")
			if err == nil || !strings.Contains(out, "matches devices") {
				t.Fatalf("ambiguous device: %v: %s", err, out)
			}

			// A virtual device sharing the MAC loses to the physical one.
			if err := os.RemoveAll(filepath.Join(f.dir, "net", "eth1", "device")); err != nil {
				t.Fatal(err)
			}

			out, err = f.run(t, shell, "parse_network_file \"$UB_MEDIA/network\" || exit 1\napply_network || exit 1\n[ \"$(net_dev e0)\" = eth0 ]\n")
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestLocalMediaValidInstallURL(t *testing.T) {
	id := strings.Repeat("0123456789abcdef", 2)
	cases := map[string]bool{
		"http://192.0.2.1:8880/bootmedia/" + id:            true,
		"http://metalman.example/bootmedia/" + id:          true,
		"https://192.0.2.1/bootmedia/" + id:                false,
		"http://192.0.2.1/bootmedia/" + id[:31]:            false,
		"http://192.0.2.1/bootmedia/" + id + "0":           false,
		"http://192.0.2.1/bootmedia/" + strings.ToUpper(id): false,
		"http://192.0.2.1/x/bootmedia/" + id:               false,
		"http:///bootmedia/" + id:                          false,
		"http://192.0.2.1/bootmedia/" + id + "/install":    false,
		"http://a$(x)/bootmedia/" + id:                     false,
	}

	for _, shell := range shells(t) {
		for url, want := range cases {
			t.Run(shell[0]+"/"+url, func(t *testing.T) {
				f := newFixture(t)

				out, err := f.run(t, shell, "valid_install_url '"+url+"'\n")
				if (err == nil) != want {
					t.Fatalf("valid_install_url(%q) = %v, want %v: %s", url, err == nil, want, out)
				}
			})
		}
	}
}

const installURL = "http://192.0.2.1:8880/bootmedia/0123456789abcdef0123456789abcdef"

func TestLocalMediaParseInstall(t *testing.T) {
	good := "IMAGE_URL=http://192.0.2.1:8880/artifacts/disk.img.gz\n" +
		"SERVE_URL=http://192.0.2.1:8880\n" +
		"DS_URL=http://192.0.2.1:8880/ds/\n" +
		"NODE_NAME=node-1\n" +
		"NODE_NAMESPACE=default\n" +
		"APISERVER_URL=https://192.0.2.5:6443\n" +
		"MEDIA_CALLBACK=" + installURL + "\n" +
		"DISK=/dev/disk/by-id/nvme-Model_SN@1\n" +
		"FUTURE_KEY=has spaces; $(ignored)\n"

	for _, shell := range shells(t) {
		t.Run(shell[0], func(t *testing.T) {
			f := newFixture(t)
			f.write(t, "install", good)

			out, err := f.run(t, shell, `
parse_install "$F/install" "`+installURL+`" || exit 1
echo "$INSTALL_IMAGE_URL|$INSTALL_SERVE_URL|$INSTALL_DS_URL|$INSTALL_NODE_NAME|$INSTALL_NODE_NAMESPACE|$INSTALL_APISERVER_URL|$INSTALL_MEDIA_CALLBACK|$INSTALL_DISK"
`)
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}

			want := "http://192.0.2.1:8880/artifacts/disk.img.gz|http://192.0.2.1:8880|http://192.0.2.1:8880/ds/|node-1|default|https://192.0.2.5:6443|" +
				installURL + "|/dev/disk/by-id/nvme-Model_SN@1\n"
			if out != want {
				t.Fatalf("got %q, want %q", out, want)
			}

			bad := map[string]string{
				"injection":         strings.Replace(good, "NODE_NAME=node-1", "NODE_NAME=$(reboot)", 1),
				"space":             strings.Replace(good, "NODE_NAME=node-1", "NODE_NAME=a b", 1),
				"carriage return":   strings.Replace(good, "NODE_NAME=node-1", "NODE_NAME=node-1\r", 1),
				"callback mismatch": strings.Replace(good, "MEDIA_CALLBACK="+installURL, "MEDIA_CALLBACK=http://192.0.2.9/bootmedia/x", 1),
				"missing image":     strings.Replace(good, "IMAGE_URL=", "OTHER=", 1),
				"malformed":         good + "garbage\n",
			}
			for name, content := range bad {
				f.write(t, "install", content)

				out, err := f.run(t, shell, "if parse_install \"$F/install\" '"+installURL+"'; then exit 0; fi\nexit 7\n")
				if err == nil {
					t.Fatalf("%s accepted: %s", name, out)
				}
			}
		})
	}
}

func TestLocalMediaFetchInstall(t *testing.T) {
	cases := []struct {
		codes string
		rc    string
	}{
		{"200", "0"},
		{"000 503 200", "0"},
		{"409", "3"},
		{"503 409", "3"},
		{"404", "1"},
		{"403", "1"},
		{"500 500 500", "1"},
	}

	for _, shell := range shells(t) {
		for _, tc := range cases {
			t.Run(shell[0]+"/"+tc.codes, func(t *testing.T) {
				f := newFixture(t)
				f.write(t, "codes", strings.ReplaceAll(tc.codes, " ", "\n")+"\n")

				out, err := f.run(t, shell, `
curl() {
    for a in "$@"; do last=$a; done
    [ "$last" = "`+installURL+`/install" ] || exit 9
    code=$(head -n 1 "$F/codes")
    sed -i 1d "$F/codes"
    printf IMAGE_URL=x > "$F/out"
    [ "$code" = 000 ] && return 7
    printf %s "$code"
}
rc=0
fetch_install "`+installURL+`" "$F/out" || rc=$?
echo "rc=$rc"
`)
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}

				if !strings.Contains(out, "rc="+tc.rc+"\n") {
					t.Fatalf("want rc=%s: %s", tc.rc, out)
				}
			})
		}
	}
}

func TestLocalMediaWaitSourceLink(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(shell[0], func(t *testing.T) {
			f := newFixture(t)
			f.write(t, "media/network", bondNetwork)
			f.netdev(t, "eth0", "02:00:00:00:00:01", true)
			f.netdev(t, "eth1", "02:00:00:00:00:02", true)
			f.write(t, "bonding/bond0", "Bonding Mode: IEEE 802.3ad Dynamic link aggregation\nMII Status: down\n")

			out, err := f.run(t, shell, `
parse_network_file "$UB_MEDIA/network" || exit 1
apply_network || exit 1
wait_source_link
`)
			if err != nil || !strings.Contains(out, "bond0: MII Status: down") || !strings.Contains(out, "trying anyway") {
				t.Fatalf("%v: %s", err, out)
			}

			f.write(t, "net/bond0/carrier", "1\n")
			f.write(t, "net/bond0/operstate", "up\n")

			out, err = f.run(t, shell, "parse_network_file \"$UB_MEDIA/network\" || exit 1\nnet_map_ethernets || exit 1\nwait_source_link\n")
			if err != nil || !strings.Contains(out, "source interface bond0 (bond0) is up") {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestLocalMediaPersistHostNetwork(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(shell[0], func(t *testing.T) {
			f := newFixture(t)
			f.write(t, "media/netplan.yaml", "network:\n  version: 2\n")
			f.write(t, "media/networkd/10-bond0.netdev", "[NetDev]\n")
			f.write(t, "media/networkd/10-enp1s0f0.link", "[Link]\n")

			// Netplan images.
			f.write(t, "root/etc/netplan/50-cloud-init.yaml", "x")

			out, err := f.run(t, shell, "persist_host_network \"$F/root\"\n")
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}

			if got := f.read(t, "root/etc/netplan/99-unbounded-metal.yaml"); got != "network:\n  version: 2\n" {
				t.Fatalf("netplan = %q", got)
			}

			if st, err := os.Stat(filepath.Join(f.dir, "root/etc/netplan/99-unbounded-metal.yaml")); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("netplan mode: %v %v", st, err)
			}

			if _, err := os.Stat(filepath.Join(f.dir, "root/etc/netplan/50-cloud-init.yaml")); !os.IsNotExist(err) {
				t.Fatal("50-cloud-init.yaml not removed")
			}

			// networkd images.
			if err := os.RemoveAll(filepath.Join(f.dir, "root/etc/netplan")); err != nil {
				t.Fatal(err)
			}

			f.write(t, "root/etc/systemd/network/90-unbounded-dhcp.network", "x")
			f.write(t, "root/etc/systemd/network/10-unbounded-metal.network", "x")

			out, err = f.run(t, shell, "persist_host_network \"$F/root\"\n")
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}

			entries, err := os.ReadDir(filepath.Join(f.dir, "root/etc/systemd/network"))
			if err != nil {
				t.Fatal(err)
			}

			var names []string
			for _, e := range entries {
				names = append(names, e.Name())

				info, err := e.Info()
				if err != nil || info.Mode().Perm() != 0o644 {
					t.Fatalf("%s mode: %v %v", e.Name(), info, err)
				}
			}

			if got := strings.Join(names, ","); got != "10-bond0.netdev,10-enp1s0f0.link" {
				t.Fatalf("networkd files = %s", got)
			}

			// Unexpected file names are refused.
			f.write(t, "media/networkd/evil.sh", "x")

			if out, err := f.run(t, shell, "if persist_host_network \"$F/root\"; then exit 0; fi\nexit 7\n"); err == nil {
				t.Fatalf("accepted unexpected file: %s", out)
			}
		})
	}
}
