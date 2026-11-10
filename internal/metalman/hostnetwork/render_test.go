// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

const referenceNetplanBody = `version: 2
ethernets:
  "enp49s0f0np0":
    match:
      macaddress: "02:00:00:00:00:01"
    set-name: "enp49s0f0np0"
    dhcp4: false
    dhcp6: false
    link-local: []
  "enp49s0f1np1":
    match:
      macaddress: "02:00:00:00:00:02"
    set-name: "enp49s0f1np1"
    dhcp4: false
    dhcp6: false
    link-local: []
bonds:
  "bond0":
    interfaces:
      - "enp49s0f0np0"
      - "enp49s0f1np1"
    parameters:
      mode: "802.3ad"
      mii-monitor-interval: 100
      transmit-hash-policy: "layer3+4"
    dhcp4: false
    dhcp6: false
    mtu: 1500
    addresses:
      - "192.0.2.30/24"
    routes:
      - to: "default"
        via: "192.0.2.1"
    nameservers:
      addresses:
        - "192.0.2.53"
        - "192.0.2.54"
vlans:
  "bond0.64":
    id: 64
    link: "bond0"
    dhcp4: false
    dhcp6: false
    addresses:
      - "198.51.100.30/24"
    routes:
      - to: "203.0.113.0/24"
        via: "198.51.100.1"
      - to: "100.64.0.0/16"
        via: "198.51.100.1"
`

var referenceNetworkd = []File{
	{"10-unbounded-enp49s0f0np0.link", []byte("[Match]\nPermanentMACAddress=02:00:00:00:00:01\n\n[Link]\nName=enp49s0f0np0\n")},
	{"10-unbounded-enp49s0f1np1.link", []byte("[Match]\nPermanentMACAddress=02:00:00:00:00:02\n\n[Link]\nName=enp49s0f1np1\n")},
	{"20-unbounded-bond0.64.netdev", []byte("[NetDev]\nName=bond0.64\nKind=vlan\n\n[VLAN]\nId=64\n")},
	{"20-unbounded-bond0.netdev", []byte("[NetDev]\nName=bond0\nKind=bond\nMTUBytes=1500\n\n[Bond]\nMode=802.3ad\nMIIMonitorSec=100ms\nTransmitHashPolicy=layer3+4\n")},
	{"30-unbounded-bond0.64.network", []byte("[Match]\nName=bond0.64\n\n[Network]\nAddress=198.51.100.30/24\n\n" +
		"[Route]\nDestination=203.0.113.0/24\nGateway=198.51.100.1\n\n[Route]\nDestination=100.64.0.0/16\nGateway=198.51.100.1\n")},
	{"30-unbounded-bond0.network", []byte("[Match]\nName=bond0\n\n[Network]\nVLAN=bond0.64\nAddress=192.0.2.30/24\nGateway=192.0.2.1\nDNS=192.0.2.53\nDNS=192.0.2.54\n")},
	{"30-unbounded-enp49s0f0np0.network", []byte("[Match]\nPermanentMACAddress=02:00:00:00:00:01\n\n[Network]\nBond=bond0\nLinkLocalAddressing=no\n")},
	{"30-unbounded-enp49s0f1np1.network", []byte("[Match]\nPermanentMACAddress=02:00:00:00:00:02\n\n[Network]\nBond=bond0\nLinkLocalAddressing=no\n")},
}

func indent(s string) string {
	var b strings.Builder

	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			b.WriteString("  " + l)
		}
	}

	return b.String()
}

func TestRenderNetplanReference(t *testing.T) {
	n, _ := reference()

	got, err := RenderNetplan(n)
	if err != nil {
		t.Fatal(err)
	}

	if want := "network:\n" + indent(referenceNetplanBody); string(got) != want {
		t.Fatalf("netplan mismatch:\n%s\nwant:\n%s", got, want)
	}

	cfg, err := RenderNetplanConfig(n)
	if err != nil {
		t.Fatal(err)
	}

	if string(cfg) != referenceNetplanBody {
		t.Fatalf("network-config mismatch:\n%s\nwant:\n%s", cfg, referenceNetplanBody)
	}
}

// TestRenderNetplanParses checks the document is valid YAML with the
// expected structure and value types.
func TestRenderNetplanParses(t *testing.T) {
	n, _ := reference()
	n.Bonds[0].LACPRate = "slow"
	n.Ethernets[0].MTU = i32(9000)

	b, err := RenderNetplan(n)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Network struct {
			Version   int `json:"version"`
			Ethernets map[string]struct {
				Match struct {
					MACAddress string `json:"macaddress"`
				} `json:"match"`
				SetName   string   `json:"set-name"`
				DHCP4     bool     `json:"dhcp4"`
				MTU       int      `json:"mtu"`
				LinkLocal []string `json:"link-local"`
			} `json:"ethernets"`
			Bonds map[string]struct {
				Interfaces []string `json:"interfaces"`
				Parameters struct {
					Mode     string `json:"mode"`
					MII      int    `json:"mii-monitor-interval"`
					Hash     string `json:"transmit-hash-policy"`
					LACPRate string `json:"lacp-rate"`
				} `json:"parameters"`
				Routes []struct {
					To  string `json:"to"`
					Via string `json:"via"`
				} `json:"routes"`
				Nameservers struct {
					Addresses []string `json:"addresses"`
				} `json:"nameservers"`
			} `json:"bonds"`
			VLANs map[string]struct {
				ID   int    `json:"id"`
				Link string `json:"link"`
			} `json:"vlans"`
		} `json:"network"`
	}

	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid yaml: %v\n%s", err, b)
	}

	nw := doc.Network
	e := nw.Ethernets["enp49s0f0np0"]

	if nw.Version != 2 || e.Match.MACAddress != "02:00:00:00:00:01" || e.SetName != "enp49s0f0np0" || e.MTU != 9000 || e.LinkLocal == nil || len(e.LinkLocal) != 0 {
		t.Errorf("unexpected ethernet: %+v", e)
	}

	bond := nw.Bonds["bond0"]
	if bond.Parameters.Mode != "802.3ad" || bond.Parameters.MII != 100 || bond.Parameters.Hash != "layer3+4" || bond.Parameters.LACPRate != "slow" {
		t.Errorf("unexpected bond parameters: %+v", bond.Parameters)
	}

	if len(bond.Routes) != 1 || bond.Routes[0].To != "default" || bond.Routes[0].Via != "192.0.2.1" {
		t.Errorf("unexpected bond routes: %+v", bond.Routes)
	}

	if strings.Join(bond.Nameservers.Addresses, ",") != "192.0.2.53,192.0.2.54" {
		t.Errorf("unexpected nameservers: %+v", bond.Nameservers)
	}

	if v := nw.VLANs["bond0.64"]; v.ID != 64 || v.Link != "bond0" {
		t.Errorf("unexpected vlan: %+v", v)
	}
}

func TestRenderNetplanDNSWithoutGateway(t *testing.T) {
	n, _ := reference()
	n.Bonds[0].Gateway = ""
	n.Bonds[0].Addresses = nil

	b, err := RenderNetplan(n)
	if err != nil {
		t.Fatal(err)
	}

	// The VLAN is the first addressed interface, so it carries DNS, and the
	// now-unaddressed bond disables link-local addressing.
	want := "      routes:\n        - to: \"203.0.113.0/24\"\n          via: \"198.51.100.1\"\n" +
		"        - to: \"100.64.0.0/16\"\n          via: \"198.51.100.1\"\n" +
		"      nameservers:\n        addresses:\n          - \"192.0.2.53\"\n          - \"192.0.2.54\"\n"
	if !strings.HasSuffix(string(b), want) {
		t.Fatalf("unexpected netplan:\n%s", b)
	}

	if strings.Count(string(b), "link-local: []") != 3 {
		t.Fatalf("expected link-local disabled on members and bond:\n%s", b)
	}
}

func TestRenderNetplanSingle(t *testing.T) {
	n, _ := single()

	b, err := RenderNetplanConfig(n)
	if err != nil {
		t.Fatal(err)
	}

	want := `version: 2
ethernets:
  "eth0":
    match:
      macaddress: "02:00:00:00:00:0a"
    set-name: "eth0"
    dhcp4: false
    dhcp6: false
    addresses:
      - "192.0.2.10/24"
    routes:
      - to: "default"
        via: "192.0.2.1"
`
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
}

func TestRenderersRejectInvalid(t *testing.T) {
	n, _ := reference()
	n.Bonds[0].Interfaces[1] = "missing"

	if _, err := RenderNetplan(n); err == nil {
		t.Error("RenderNetplan: expected error")
	}

	if _, err := RenderNetplanConfig(n); err == nil {
		t.Error("RenderNetplanConfig: expected error")
	}

	if _, err := RenderNetworkd(n); err == nil {
		t.Error("RenderNetworkd: expected error")
	}

	if _, err := RenderNetworkd(nil); err == nil {
		t.Error("RenderNetworkd(nil): expected error")
	}

	_, l := reference()
	if _, err := MediaCPIO(n, l); err == nil {
		t.Error("MediaCPIO: expected error")
	}
}

func TestRenderNetworkdReference(t *testing.T) {
	n, _ := reference()

	got, err := RenderNetworkd(n)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(referenceNetworkd) {
		t.Fatalf("got %d files, want %d", len(got), len(referenceNetworkd))
	}

	for i, w := range referenceNetworkd {
		if got[i].Name != w.Name || !bytes.Equal(got[i].Data, w.Data) {
			t.Errorf("file %d = %s:\n%s\nwant %s:\n%s", i, got[i].Name, got[i].Data, w.Name, w.Data)
		}
	}
}

func TestRenderNetworkdOptionals(t *testing.T) {
	n, _ := reference()
	n.Bonds[0].LACPRate = "fast"
	n.Bonds[0].MIIMonitorInterval = i32(0)
	n.Bonds[0].MTU = nil
	n.VLANs[0].MTU = i32(1400)
	n.Ethernets = append(n.Ethernets, v1alpha3.EthernetSpec{
		Name:       "eno1",
		MACAddress: "02:00:00:00:00:03",
		InterfaceAddressing: v1alpha3.InterfaceAddressing{
			MTU: i32(9000),
		},
	})
	n.VLANs = append(n.VLANs, v1alpha3.VLANSpec{Name: "eno1.10", ID: 10, Link: "eno1"})

	files, err := RenderNetworkd(n)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]string{}
	for _, f := range files {
		byName[f.Name] = string(f.Data)
	}

	checks := map[string]string{
		"20-unbounded-bond0.netdev":     "[NetDev]\nName=bond0\nKind=bond\n\n[Bond]\nMode=802.3ad\nMIIMonitorSec=0ms\nTransmitHashPolicy=layer3+4\nLACPTransmitRate=fast\n",
		"20-unbounded-bond0.64.netdev":  "[NetDev]\nName=bond0.64\nKind=vlan\nMTUBytes=1400\n\n[VLAN]\nId=64\n",
		"30-unbounded-eno1.network":     "[Match]\nPermanentMACAddress=02:00:00:00:00:03\n\n[Link]\nMTUBytes=9000\n\n[Network]\nLinkLocalAddressing=no\nVLAN=eno1.10\n",
		"30-unbounded-eno1.10.network":  "[Match]\nName=eno1.10\n\n[Network]\nLinkLocalAddressing=no\n",
		"10-unbounded-eno1.link":        "[Match]\nPermanentMACAddress=02:00:00:00:00:03\n\n[Link]\nName=eno1\n",
		"20-unbounded-eno1.10.netdev":   "[NetDev]\nName=eno1.10\nKind=vlan\n\n[VLAN]\nId=10\n",
		"30-unbounded-bond0.64.network": "[Match]\nName=bond0.64\n\n[Network]\nAddress=198.51.100.30/24\n\n[Route]\nDestination=203.0.113.0/24\nGateway=198.51.100.1\n\n[Route]\nDestination=100.64.0.0/16\nGateway=198.51.100.1\n",
	}
	for name, want := range checks {
		if byName[name] != want {
			t.Errorf("%s =\n%s\nwant:\n%s", name, byName[name], want)
		}
	}

	for i := 1; i < len(files); i++ {
		if files[i-1].Name >= files[i].Name {
			t.Fatalf("files not sorted: %s >= %s", files[i-1].Name, files[i].Name)
		}
	}
}

func TestMediaCPIO(t *testing.T) {
	n, l := reference()

	b, err := MediaCPIO(n, l)
	if err != nil {
		t.Fatal(err)
	}

	again, err := MediaCPIO(n, l)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b, again) {
		t.Fatal("media.cpio is not deterministic")
	}

	entries := readCPIO(t, b)

	netplan, err := RenderNetplan(n)
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		name string
		mode uint32
		data string
	}

	wants := []want{
		{"unbounded", 0o40755, ""},
		{"unbounded/media", 0o40755, ""},
		{"unbounded/media/network", 0o100644, referenceRuntime},
		{"unbounded/media/netplan.yaml", 0o100644, string(netplan)},
		{"unbounded/media/networkd", 0o40755, ""},
	}
	for _, f := range referenceNetworkd {
		wants = append(wants, want{"unbounded/media/networkd/" + f.Name, 0o100644, string(f.Data)})
	}

	if len(entries) != len(wants) {
		t.Fatalf("got %d entries, want %d", len(entries), len(wants))
	}

	for i, w := range wants {
		e := entries[i]
		if e.name != w.name || e.mode != w.mode || string(e.data) != w.data || e.uid != 0 || e.gid != 0 || e.mtime != 0 {
			t.Errorf("entry %d = %s %o, want %s %o", i, e.name, e.mode, w.name, w.mode)
		}
	}

	if _, err := MediaCPIO(n, v1alpha3.DHCPLease{}); err == nil {
		t.Fatal("expected lease validation error")
	}
}

func TestMediaCPIOWithSystemCPIO(t *testing.T) {
	bin, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("cpio not installed")
	}

	n, l := reference()

	b, err := MediaCPIO(n, l)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	p := filepath.Join(dir, "media.cpio")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-id", "--quiet", "--no-absolute-filenames")
	cmd.Stdin, cmd.Dir = f, out

	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio -i: %v: %s", err, msg)
	}

	got, err := os.ReadFile(filepath.Join(out, MediaRuntimePath))
	if err != nil || string(got) != referenceRuntime {
		t.Fatalf("extracted runtime = %q, %v", got, err)
	}

	link, err := os.ReadFile(filepath.Join(out, MediaNetworkdPath, "20-unbounded-bond0.netdev"))
	if err != nil || !strings.Contains(string(link), "Kind=bond") {
		t.Fatalf("extracted netdev = %q, %v", link, err)
	}
}
