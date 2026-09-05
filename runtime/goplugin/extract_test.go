package goplugin

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"GoCraft/core/plugin"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func TestExtractExecutableAndCleanup(t *testing.T) {
	bundlePath := writeTestBundle(t, "bin/example", []byte("native plugin"))
	runtime := New(Config{ExtractDirectory: t.TempDir()})
	bundle := plugin.Bundle{Bundle: gcpkg.Bundle{Path: bundlePath, Manifest: gcpkg.Manifest{ID: "example", Entry: "bin/example"}}}
	executable, cleanup, err := runtime.extract(bundle)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil || string(data) != "native plugin" {
		t.Fatalf("extracted executable = %q, %v", data, err)
	}
	directory := filepath.Dir(executable)
	cleanup()
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("cleanup left directory: %v", err)
	}
}

// The bug that stopped the first Go plugin ever loaded on Windows.
//
// os/exec resolves a command through PATHEXT even when handed a full path, so
// an extensionless PE image fails with "executable file not found in %PATH%" —
// which sends whoever reads it looking at their PATH. The entry in the bundle
// is what an author named; the extracted file is the host's business.
func TestExtractedNameIsRunnableOnItsOperatingSystem(t *testing.T) {
	for _, testCase := range []struct {
		entry, goos, want string
	}{
		{"bin/example", "windows", "plugin.exe"},
		{"bin/example.exe", "windows", "plugin.exe"},
		{"bin/example.EXE", "windows", "plugin.EXE"},
		{"bin/example", "linux", "plugin"},
		{"bin/example.exe", "linux", "plugin.exe"},
	} {
		if got := executableName(testCase.entry, testCase.goos); got != testCase.want {
			t.Fatalf("executableName(%q, %q) = %q, want %q",
				testCase.entry, testCase.goos, got, testCase.want)
		}
	}
}

func TestExtractRejectsUnsafeOrMissingEntry(t *testing.T) {
	bundlePath := writeTestBundle(t, "bin/example", []byte("plugin"))
	runtime := New(Config{ExtractDirectory: t.TempDir()})
	for _, entry := range []string{"", "../example", `bin\example`, "bin/missing"} {
		bundle := plugin.Bundle{Bundle: gcpkg.Bundle{Path: bundlePath, Manifest: gcpkg.Manifest{ID: "example", Entry: entry}}}
		if _, _, err := runtime.extract(bundle); err == nil {
			t.Fatalf("entry %q was accepted", entry)
		}
	}
}

func writeTestBundle(t *testing.T, entry string, content []byte) string {
	return writeTestBundleWith(t, entry, content, nil)
}

// writeTestBundleWith packs an executable and, when one is given, the command
// tree a plugin registers its handlers against.
func writeTestBundleWith(t *testing.T, entry string, content, commandTree []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "example.gcpkg")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	writer, err := archive.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if commandTree != nil {
		trees, err := archive.Create(commandTreeEntry)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := trees.Write(commandTree); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}
