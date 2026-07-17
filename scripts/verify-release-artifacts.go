// Command verify-release-artifacts checks the public contract of a GoReleaser
// snapshot or release directory.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	GoOS   string `json:"goos"`
	GoArch string `json:"goarch"`
	Type   string `json:"type"`
}

type target struct {
	goos   string
	goarch string
}

var releaseTargets = []target{
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
}

func main() {
	dist := "dist"
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/verify-release-artifacts.go [dist-directory]")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		dist = os.Args[1]
	}
	if err := verify(dist); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("release artifacts verified: four static targets, archives, and checksums")
}

func verify(dist string) error {
	contents, err := os.ReadFile(filepath.Join(dist, "artifacts.json"))
	if err != nil {
		return fmt.Errorf("read artifacts: %w", err)
	}
	var artifacts []artifact
	if err := json.Unmarshal(contents, &artifacts); err != nil {
		return fmt.Errorf("decode artifacts: %w", err)
	}
	checksums, err := readChecksums(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return err
	}

	for _, expected := range releaseTargets {
		binary, err := exactlyOne(artifacts, expected, "Binary")
		if err != nil {
			return err
		}
		if err := verifyCGODisabled(binary.Path); err != nil {
			return fmt.Errorf("%s/%s binary: %w", expected.goos, expected.goarch, err)
		}
		archive, err := exactlyOne(artifacts, expected, "Archive")
		if err != nil {
			return err
		}
		if err := verifyChecksum(archive.Path, checksums[filepath.Base(archive.Path)]); err != nil {
			return err
		}
		if err := verifyArchive(archive.Path); err != nil {
			return err
		}
	}
	return nil
}

func exactlyOne(artifacts []artifact, expected target, artifactType string) (artifact, error) {
	var matches []artifact
	for _, candidate := range artifacts {
		if candidate.GoOS == expected.goos && candidate.GoArch == expected.goarch && candidate.Type == artifactType {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return artifact{}, fmt.Errorf(
			"%s/%s has %d %s artifacts, want 1", expected.goos, expected.goarch, len(matches), artifactType,
		)
	}
	return matches[0], nil
}

func verifyCGODisabled(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Go build information: %w", err)
	}
	for _, setting := range info.Settings {
		if setting.Key != "CGO_ENABLED" {
			continue
		}
		if setting.Value != "0" {
			return fmt.Errorf("CGO_ENABLED = %q, want %q", setting.Value, "0")
		}
		return nil
	}
	return fmt.Errorf("Go build information has no CGO_ENABLED setting")
}

func readChecksums(path string) (map[string]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	checksums := make(map[string]string)
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums line %d is malformed", lineNumber+1)
		}
		checksums[fields[1]] = fields[0]
	}
	return checksums, nil
}

func verifyChecksum(path, want string) error {
	if want == "" {
		return fmt.Errorf("checksum is missing for %s", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("hash %s: %w", filepath.Base(path), err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("checksum for %s = %s, want %s", filepath.Base(path), got, want)
	}
	return nil
}

func verifyArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", filepath.Base(path), err)
	}
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("open gzip archive %s: %w", filepath.Base(path), err)
	}
	found := map[string]bool{
		"aethos":                        false,
		"LICENSE":                       false,
		"README.md":                     false,
		"deploy/systemd/aethos.service": false,
	}
	reader := tar.NewReader(gzipReader)
	for {
		header, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = gzipReader.Close()
			_ = file.Close()
			return fmt.Errorf("read archive %s: %w", filepath.Base(path), readErr)
		}
		if _, wanted := found[header.Name]; wanted {
			found[header.Name] = true
		}
	}
	if err := errors.Join(gzipReader.Close(), file.Close()); err != nil {
		return fmt.Errorf("close archive %s: %w", filepath.Base(path), err)
	}
	for name, present := range found {
		if !present {
			return fmt.Errorf("archive %s is missing %s", filepath.Base(path), name)
		}
	}
	return nil
}
