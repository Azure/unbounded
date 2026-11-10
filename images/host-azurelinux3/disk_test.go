// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostazurelinux3_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover only the settings the disk script rejects before it reads
// any input.
func TestDiskRejectsUnsupportedSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     []string
		wantErr string
	}{
		{name: "missing architecture", env: nil, wantErr: "TARGETARCH is required"},
		{name: "unknown architecture", env: []string{"TARGETARCH=riscv64"}, wantErr: "unsupported architecture: riscv64"},
		{name: "unknown distribution", env: []string{"TARGETARCH=amd64", "DISTRO=fedora"}, wantErr: "unsupported distribution: fedora"},
		{name: "MSHV on Ubuntu", env: []string{"TARGETARCH=amd64", "DISTRO=ubuntu", "MSHV=1"}, wantErr: "MSHV images support Azure Linux only, not ubuntu"},
		{name: "MSHV on arm64", env: []string{"TARGETARCH=arm64", "MSHV=1"}, wantErr: "MSHV images support amd64 only, not arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("/bin/sh", "assets/disk.sh")
			command.Env = append([]string{"PATH=/usr/bin:/bin"}, test.env...)

			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.wantErr) {
				t.Fatalf("disk.sh error = %v, want %q\n%s", err, test.wantErr, output)
			}
		})
	}
}

// fakeTools stand in for the tools that need real file systems or block
// devices. They record their inputs under $CAPTURE: the ESP contents in esp/,
// the root file system in root/, and the partition table in sfdisk.txt.
var fakeTools = map[string]string{
	"mkfs.vfat": `mkdir -p "$CAPTURE/esp"`,
	"mmd": `while [ $# -gt 0 ]; do
  case "$1" in
    -i) shift 2 ;;
    ::/*) mkdir "$CAPTURE/esp/${1#::/}" || exit 1; shift ;;
    *) shift ;;
  esac
done`,
	"mcopy": `sources=""
while [ $# -gt 1 ]; do
  case "$1" in
    -s) shift ;;
    -i) shift 2 ;;
    *) sources="$sources
$1"; shift ;;
  esac
done
dest="$CAPTURE/esp/${1#::/}"
printf '%s\n' "$sources" | while IFS= read -r source; do
  [ -n "$source" ] || continue
  case "$dest" in
    */) cp -r "$source" "$dest" || exit 1 ;;
    *) [ -d "$(dirname "$dest")" ] || exit 1; cp "$source" "$dest" || exit 1 ;;
  esac
done`,
	"mkfs.ext4": `while [ $# -gt 0 ]; do
  case "$1" in
    -U) echo "$2" > "$CAPTURE/root-uuid"; shift 2 ;;
    -d) cp -a "$2" "$CAPTURE/root"; shift 2 ;;
    *) shift ;;
  esac
done`,
	"sfdisk": `cat > "$CAPTURE/sfdisk.txt"`,
	"dd":     `:`,
	"pigz":   `echo fake-disk`,
}

func TestDiskBuildsLayouts(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not installed")
	}

	const kver = "6.6.0-test"

	for _, test := range []struct {
		name, arch, distro, mshv string
	}{
		{name: "Ubuntu amd64", arch: "amd64", distro: "ubuntu"},
		{name: "Azure Linux amd64", arch: "amd64"},
		{name: "Azure Linux arm64", arch: "arm64"},
		{name: "Azure Linux amd64 MSHV", arch: "amd64", mshv: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "capture")
			output := filepath.Join(dir, "out", "disk.img.gz")
			path := buildFakeTools(t, filepath.Join(dir, "bin"))
			tarball := buildRootfs(t, filepath.Join(dir, "rootfs"), kver, test.distro == "ubuntu")
			kverFile := filepath.Join(dir, "rootfs.kver")
			writeFile(t, kverFile, kver+"\n", 0o644)
			mustMkdir(t, capture)

			command := exec.Command("/bin/sh", "assets/disk.sh")
			command.Env = []string{
				"PATH=" + path, "CAPTURE=" + capture,
				"TARGETARCH=" + test.arch, "DISTRO=" + test.distro, "MSHV=" + test.mshv,
				"ROOTFS_TAR=" + tarball, "ROOTFS_KVER=" + kverFile,
				"WORK=" + filepath.Join(dir, "work"), "OUTPUT=" + output,
			}

			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("disk.sh: %v\n%s", err, out)
			}

			if got := readFile(t, output); got != "fake-disk\n" {
				t.Errorf("output = %q, want the pigz output", got)
			}

			if _, err := os.Stat(filepath.Join(dir, "work")); !os.IsNotExist(err) {
				t.Errorf("work directory was not removed: %v", err)
			}

			esp := filepath.Join(capture, "esp")
			root := filepath.Join(capture, "root")
			rootUUID := strings.TrimSpace(readFile(t, filepath.Join(capture, "root-uuid")))

			assertContains(t, readFile(t, filepath.Join(esp, "EFI/BOOT/BOOTX64.EFI")), "shim")
			assertContains(t, readFile(t, filepath.Join(root, "etc/fstab")), "UUID="+rootUUID+" / ext4 ")

			if link, err := os.Readlink(filepath.Join(root, "etc/resolv.conf")); err != nil || link != "../run/systemd/resolve/stub-resolv.conf" {
				t.Errorf("resolv.conf link = %q, %v", link, err)
			}

			if entries, err := os.ReadDir(filepath.Join(root, "boot/efi")); err != nil || len(entries) != 0 {
				t.Errorf("root boot/efi = %v, %v, want empty", entries, err)
			}

			partitions := readFile(t, filepath.Join(capture, "sfdisk.txt"))
			if n := strings.Count(partitions, "start="); n != 2 {
				t.Errorf("sfdisk partitions = %d, want 2\n%s", n, partitions)
			}

			assertContains(t, partitions, "type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B")

			console := " console=tty0 console=ttyS0,115200n8"
			if test.arch == "arm64" {
				console = ""
			}

			linux := "linux /boot/vmlinuz-" + kver + " root=UUID=" + rootUUID + " rw" + console

			if test.distro == "ubuntu" {
				bootCfg := readFile(t, filepath.Join(root, "boot/grub/grub.cfg"))
				assertContains(t, bootCfg, linux)
				assertContains(t, bootCfg, "initrd /boot/initrd.img-"+kver+"\n")

				for _, name := range []string{"EFI/ubuntu/grub.cfg", "EFI/BOOT/grub.cfg"} {
					cfg := readFile(t, filepath.Join(esp, name))
					assertContains(t, cfg, "search --no-floppy --fs-uuid --set=root "+rootUUID+"\n")
					assertContains(t, cfg, "set prefix=($root)/boot/grub\n")
					assertContains(t, cfg, "configfile $prefix/grub.cfg\n")
				}

				assertMissing(t, filepath.Join(esp, "boot/grub2"))

				return
			}

			assertMissing(t, filepath.Join(root, "boot/grub/grub.cfg"))

			for _, name := range []string{"boot/grub2/grub.cfg", "EFI/BOOT/grub.cfg"} {
				cfg := readFile(t, filepath.Join(esp, name))
				assertContains(t, cfg, linux)
				assertContains(t, cfg, "initrd /boot/initramfs-"+kver+".img\n")

				if console == "" && strings.Contains(cfg, "console=") {
					t.Errorf("%s sets a console on arm64:\n%s", name, cfg)
				}

				chainloader := strings.Index(cfg, "chainloader /HvLoader.efi")
				if test.mshv == "1" && (chainloader < 0 || chainloader > strings.Index(cfg, linux)) {
					t.Errorf("%s does not chainload HvLoader.efi before Linux:\n%s", name, cfg)
				}

				if test.mshv != "1" && chainloader >= 0 {
					t.Errorf("%s chainloads HvLoader.efi without MSHV:\n%s", name, cfg)
				}
			}
		})
	}
}

// buildFakeTools writes the fake tools to dir and returns a PATH that finds
// them before the real tools.
func buildFakeTools(t *testing.T, dir string) string {
	t.Helper()
	mustMkdir(t, dir)

	for name, body := range fakeTools {
		writeFile(t, filepath.Join(dir, name), "#!/bin/sh\nset -eu\n"+body+"\n", 0o755)
	}

	return dir + ":/usr/bin:/bin"
}

// buildRootfs lays out a minimal root file system in dir and archives it.
func buildRootfs(t *testing.T, dir, kver string, ubuntu bool) string {
	t.Helper()

	files := map[string]string{
		"etc/os-release":                 "ID=test\n",
		"boot/vmlinuz-" + kver:           "kernel\n",
		"boot/efi/EFI/BOOT/BOOTX64.EFI":  "shim\n",
		"usr/lib/modules/" + kver + "/x": "module\n",
	}
	if ubuntu {
		files["boot/initrd.img-"+kver] = "initrd\n"
		files["boot/efi/EFI/ubuntu/grubx64.efi"] = "grub\n"
	} else {
		files["boot/initramfs-"+kver+".img"] = "initrd\n"
	}

	for name, content := range files {
		mustMkdir(t, filepath.Dir(filepath.Join(dir, name)))
		writeFile(t, filepath.Join(dir, name), content, 0o644)
	}

	tarball := dir + ".tar"
	if out, err := exec.Command("tar", "--create", "--file", tarball, "-C", dir, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}

	return tarball
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}

	return string(content)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()

	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func assertMissing(t *testing.T, name string) {
	t.Helper()

	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("%s exists or is unreadable: %v", name, err)
	}
}
