package goplugin

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"GoCraft/core/plugin"
)

const maximumExecutableSize = 256 << 20

func (r *Runtime) extract(bundle plugin.Bundle) (string, func(), error) {
	entry := bundle.Manifest.Entry
	if !validEntry(entry) {
		return "", nil, fmt.Errorf("go runtime: plugin %s has invalid entry %q", bundle.Manifest.ID, entry)
	}
	archive, err := zip.OpenReader(bundle.Path)
	if err != nil {
		return "", nil, fmt.Errorf("go runtime: open bundle: %w", err)
	}
	defer archive.Close()
	var source *zip.File
	for _, file := range archive.File {
		name := path.Clean(strings.ReplaceAll(file.Name, "\\", "/"))
		if name == entry {
			if source != nil {
				return "", nil, fmt.Errorf("go runtime: duplicate entry %s", entry)
			}
			source = file
		}
	}
	if source == nil || source.FileInfo().IsDir() {
		return "", nil, fmt.Errorf("go runtime: bundle is missing executable %s", entry)
	}
	if source.UncompressedSize64 > maximumExecutableSize {
		return "", nil, fmt.Errorf("go runtime: executable exceeds %d bytes", maximumExecutableSize)
	}
	directory, err := os.MkdirTemp(r.config.ExtractDirectory, "gocraft-go-")
	if err != nil {
		return "", nil, fmt.Errorf("go runtime: create extraction directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	target := filepath.Join(directory, executableName(entry, runtime.GOOS))
	if err := copyExecutable(source, target); err != nil {
		cleanup()
		return "", nil, err
	}
	return target, cleanup, nil
}

func copyExecutable(source *zip.File, target string) error {
	reader, err := source.Open()
	if err != nil {
		return fmt.Errorf("go runtime: open executable: %w", err)
	}
	defer reader.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("go runtime: create executable: %w", err)
	}
	written, copyErr := io.Copy(output, io.LimitReader(reader, maximumExecutableSize+1))
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("go runtime: extract executable: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("go runtime: close executable: %w", closeErr)
	}
	if written > maximumExecutableSize {
		return fmt.Errorf("go runtime: executable expanded beyond its size limit")
	}
	return nil
}

// executableName is what the extracted binary has to be called for this
// operating system to run it.
//
// The manifest names a path inside the archive; what the file is called once
// it is out is the host's business, and on Windows that is not cosmetic.
// os/exec resolves a command through PATHEXT even when it was handed a full
// path, so a perfectly valid PE image with no extension fails with "executable
// file not found in %PATH%" — a message that sends whoever reads it looking at
// their PATH, which has nothing to do with it.
//
// An entry that already ends in .exe keeps it rather than gaining a second one.
// Elsewhere the name is left alone: a bundle built on Windows and run on Linux
// extracts as plugin.exe and runs perfectly well, because nothing there reads
// the extension.
func executableName(entry, goos string) string {
	name := "plugin" + path.Ext(entry)
	if goos == "windows" && !strings.EqualFold(path.Ext(name), ".exe") {
		name += ".exe"
	}
	return name
}

func validEntry(entry string) bool {
	if entry == "" || strings.Contains(entry, `\`) || path.IsAbs(entry) {
		return false
	}
	cleaned := path.Clean(entry)
	return cleaned == entry && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}
