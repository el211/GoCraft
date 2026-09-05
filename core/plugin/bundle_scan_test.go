package plugin

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func writeBundle(t *testing.T, directory, name, manifest string, extra map[string]string) {
	t.Helper()
	file, err := os.Create(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := map[string]string{"plugin.toml": manifest}
	for path, contents := range extra {
		entries[path] = contents
	}
	for path, contents := range entries {
		entry, err := archive.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanBundlesReadsAndSortsManifests(t *testing.T) {
	directory := t.TempDir()
	writeBundle(t, directory, "z-shop.gcpkg", `
id = "fr.oreo.shop"
version = "1.2.0"
api = 1
runtime = "go"
[[subscribe]]
event = "block.break"
perms = ["shop.use"]

[[subscribe]]
event = "player.join"
`, nil)
	writeBundle(t, directory, "a-protect.gcpkg", `
id = "fr.oreo.protect"
version = "1.0.0"
api = 1
runtime = "go"
`, map[string]string{"payload/plugin": "binary"})
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundles, err := ScanBundles(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 2 {
		t.Fatalf("bundle count = %d", len(bundles))
	}
	if bundles[0].Manifest.ID != "fr.oreo.protect" || bundles[1].Manifest.ID != "fr.oreo.shop" {
		t.Fatalf("bundle order = %s, %s", bundles[0].Manifest.ID, bundles[1].Manifest.ID)
	}
	shop := bundles[1].Manifest
	if len(shop.Subscriptions) != 2 || shop.Subscriptions[0].Priority != gcpkg.PriorityNormal {
		t.Fatalf("subscriptions = %+v", shop.Subscriptions)
	}
	if len(shop.Permissions) != 1 || shop.Permissions[0] != "shop.use" {
		t.Fatalf("permissions = %v", shop.Permissions)
	}
}

func TestScanBundlesAllowsMissingDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	bundles, err := ScanBundles(directory)
	if err != nil || len(bundles) != 0 {
		t.Fatalf("ScanBundles() = %v, %v", bundles, err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("plugin directory was not created: %v", err)
	}
}
