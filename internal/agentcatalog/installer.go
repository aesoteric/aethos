package agentcatalog

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	maxArchiveBytes   = 256 << 20
	maxExtractedBytes = 1 << 30
)

// CurrentPlatform returns the ACP registry key for the running OS and CPU.
func CurrentPlatform() (string, error) {
	var operatingSystem string
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		operatingSystem = runtime.GOOS
	default:
		return "", fmt.Errorf("unsupported operating system %q", runtime.GOOS)
	}
	var architecture string
	switch runtime.GOARCH {
	case "arm64":
		architecture = "aarch64"
	case "amd64":
		architecture = "x86_64"
	default:
		return "", fmt.Errorf("unsupported architecture %q", runtime.GOARCH)
	}
	return operatingSystem + "-" + architecture, nil
}

func installBinary(
	ctx context.Context,
	client *http.Client,
	agentsDir, id string,
	distribution BinaryDistribution,
) (_ string, returnErr error) {
	if client == nil {
		client = http.DefaultClient
	}
	archiveURL := strings.TrimSpace(distribution.Archive)
	if archiveURL == "" {
		return "", fmt.Errorf("binary archive URL is required")
	}
	if strings.TrimSpace(distribution.Command) == "" {
		return "", fmt.Errorf("binary command is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", fmt.Errorf("download binary archive: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download binary archive: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, annotate("close binary archive response", response.Body.Close()))
	}()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("download binary archive: unexpected HTTP status %s", response.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return "", fmt.Errorf("download binary archive: %w", err)
	}
	if len(payload) > maxArchiveBytes {
		return "", fmt.Errorf("download binary archive: archive exceeds %d bytes", maxArchiveBytes)
	}
	if checksum := strings.TrimSpace(distribution.SHA256); checksum != "" {
		actual := fmt.Sprintf("%x", sha256.Sum256(payload))
		if !strings.EqualFold(actual, checksum) {
			return "", fmt.Errorf("verify binary archive checksum: got %s, want %s", actual, checksum)
		}
	}
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		return "", fmt.Errorf("create Agent installation directory: %w", err)
	}
	finalDir := filepath.Join(agentsDir, id)
	if _, err := os.Stat(finalDir); err == nil {
		return "", fmt.Errorf("installation directory %q already exists", finalDir)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect installation directory %q: %w", finalDir, err)
	}
	temporaryDir, err := os.MkdirTemp(agentsDir, ".install-"+id+"-*")
	if err != nil {
		return "", fmt.Errorf("create temporary installation directory: %w", err)
	}
	defer func() {
		if returnErr != nil {
			_ = os.RemoveAll(temporaryDir)
		}
	}()
	if err := extractArchive(payload, archiveURL, distribution.Command, temporaryDir); err != nil {
		return "", err
	}
	temporaryCommand, err := containedPath(temporaryDir, distribution.Command)
	if err != nil {
		return "", fmt.Errorf("resolve binary command: %w", err)
	}
	info, err := os.Stat(temporaryCommand)
	if err != nil {
		return "", fmt.Errorf("find binary command %q after extraction: %w", distribution.Command, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("binary command %q is not a regular file", distribution.Command)
	}
	if err := os.Chmod(temporaryCommand, info.Mode().Perm()|0o700); err != nil {
		return "", fmt.Errorf("make binary command executable: %w", err)
	}
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		return "", fmt.Errorf("finish Agent installation: %w", err)
	}
	return containedPath(finalDir, distribution.Command)
}

func extractArchive(payload []byte, archiveURL, command, destination string) error {
	parsed, err := url.Parse(archiveURL)
	if err != nil {
		return fmt.Errorf("parse binary archive URL: %w", err)
	}
	path := strings.ToLower(parsed.Path)
	switch {
	case strings.HasSuffix(path, ".tar.gz"), strings.HasSuffix(path, ".tgz"):
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("open gzip binary archive: %w", err)
		}
		defer reader.Close()
		return extractTar(tar.NewReader(reader), destination)
	case strings.HasSuffix(path, ".tar.bz2"), strings.HasSuffix(path, ".tbz2"):
		return extractTar(tar.NewReader(bzip2.NewReader(bytes.NewReader(payload))), destination)
	case strings.HasSuffix(path, ".zip"):
		reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
		if err != nil {
			return fmt.Errorf("open zip binary archive: %w", err)
		}
		return extractZip(reader, destination)
	default:
		commandPath, err := containedPath(destination, command)
		if err != nil {
			return fmt.Errorf("resolve raw binary command: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(commandPath), 0o700); err != nil {
			return fmt.Errorf("create raw binary directory: %w", err)
		}
		if err := os.WriteFile(commandPath, payload, 0o700); err != nil {
			return fmt.Errorf("write raw binary: %w", err)
		}
		return nil
	}
}

func extractTar(reader *tar.Reader, destination string) error {
	var extracted int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar binary archive: %w", err)
		}
		path, err := containedPath(destination, header.Name)
		if err != nil {
			return fmt.Errorf("unsafe tar entry %q: %w", header.Name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create archive directory %q: %w", header.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || extracted > maxExtractedBytes-header.Size {
				return fmt.Errorf("extract tar binary archive: contents exceed %d bytes", maxExtractedBytes)
			}
			extracted += header.Size
			if err := writeArchiveFile(path, io.LimitReader(reader, header.Size), os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("extract tar entry %q: %w", header.Name, err)
			}
		default:
			return fmt.Errorf("extract tar binary archive: unsupported entry %q", header.Name)
		}
	}
}

func extractZip(reader *zip.Reader, destination string) error {
	var extracted uint64
	for _, entry := range reader.File {
		path, err := containedPath(destination, entry.Name)
		if err != nil {
			return fmt.Errorf("unsafe zip entry %q: %w", entry.Name, err)
		}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extract zip binary archive: unsupported symlink %q", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create archive directory %q: %w", entry.Name, err)
			}
			continue
		}
		if entry.UncompressedSize64 > maxExtractedBytes || extracted > maxExtractedBytes-entry.UncompressedSize64 {
			return fmt.Errorf("extract zip binary archive: contents exceed %d bytes", maxExtractedBytes)
		}
		extracted += entry.UncompressedSize64
		contents, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %q: %w", entry.Name, err)
		}
		writeErr := writeArchiveFile(path, io.LimitReader(contents, int64(entry.UncompressedSize64)), entry.Mode())
		closeErr := contents.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return fmt.Errorf("extract zip entry %q: %w", entry.Name, err)
		}
	}
	return nil
}

func writeArchiveFile(path string, contents io.Reader, mode os.FileMode) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	_, err = io.Copy(file, contents)
	return err
}

func containedPath(root, name string) (string, error) {
	normalized := strings.ReplaceAll(name, `\`, "/")
	cleaned := filepath.Clean(filepath.FromSlash(normalized))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the installation directory", name)
	}
	path := filepath.Join(root, cleaned)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the installation directory", name)
	}
	return path, nil
}
