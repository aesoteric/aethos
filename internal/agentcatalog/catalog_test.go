package agentcatalog_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/agentcatalog"
)

func TestRegistryListsAgentsFromHTTPBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
  "version": "1.0.0",
  "agents": [
    {
      "id": "codex-acp",
      "name": "Codex",
      "version": "1.1.4",
      "description": "ACP integration for Codex",
      "distribution": {"npx": {"package": "@agentclientprotocol/codex-acp@1.1.4"}}
    },
    {
      "id": "goose",
      "name": "goose",
      "version": "1.43.0",
      "description": "An open source coding Agent",
      "distribution": {"binary": {"linux-x86_64": {"archive": "https://example.com/goose.tgz", "cmd": "./goose"}}}
    }
  ]
}`)
	}))
	t.Cleanup(server.Close)

	registry := agentcatalog.NewRegistry(server.URL, server.Client())
	agents, err := registry.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("registry Agents = %#v, want two", agents)
	}
	if agents[0].Name != "Codex" || agents[0].Type() != "npx" || agents[0].Description != "ACP integration for Codex" {
		t.Errorf("first registry Agent = %#v, want Codex npx metadata", agents[0])
	}
	if agents[1].Type() != "binary" {
		t.Errorf("second registry Agent type = %q, want binary", agents[1].Type())
	}
}

func TestRegistryUnreachableReturnsClearError(t *testing.T) {
	registry := agentcatalog.NewRegistry("http://127.0.0.1:1/registry.json", &http.Client{})
	_, err := registry.List(t.Context())
	if err == nil || !strings.Contains(err.Error(), "fetch ACP Agent registry") {
		t.Fatalf("List error = %v, want clear registry fetch error", err)
	}
}

func TestCatalogPersistsInstalledNPXAgent(t *testing.T) {
	dataDir := t.TempDir()
	catalogPath := filepath.Join(dataDir, "agents.json")
	agentsDir := filepath.Join(dataDir, "agents")

	catalog, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = catalog.Install(t.Context(), agentcatalog.RegistryAgent{
		ID:          "codex-acp",
		Name:        "Codex",
		Version:     "1.1.4",
		Description: "ACP integration for Codex",
		Distribution: agentcatalog.Distribution{
			NPX: &agentcatalog.PackageDistribution{
				Package: "@agentclientprotocol/codex-acp@1.1.4",
				Args:    []string{"--stdio"},
				Env:     map[string]string{"CODEX_MODE": "acp"},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	reopened, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	installed, err := reopened.Resolve("codex-acp")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if installed.Name != "Codex" || installed.Type != agentcatalog.NPX {
		t.Errorf("installed Agent = %#v, want persisted Codex npx entry", installed)
	}
	if installed.Command != "npx" {
		t.Errorf("Command = %q, want npx", installed.Command)
	}
	if want := []string{"--yes", "@agentclientprotocol/codex-acp@1.1.4", "--stdio"}; !slices.Equal(installed.Args, want) {
		t.Errorf("Args = %q, want %q", installed.Args, want)
	}
	if installed.Env["CODEX_MODE"] != "acp" {
		t.Errorf("Env = %q, want persisted CODEX_MODE", installed.Env)
	}
}

func TestOpenCatalogSeesAgentInstalledByAnotherProcess(t *testing.T) {
	dataDir := t.TempDir()
	catalogPath := filepath.Join(dataDir, "agents.json")
	agentsDir := filepath.Join(dataDir, "agents")
	runningCatalog, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("open running catalog: %v", err)
	}
	installerCatalog, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("open installer catalog: %v", err)
	}
	_, err = installerCatalog.Install(t.Context(), agentcatalog.RegistryAgent{
		ID:      "codex-acp",
		Name:    "Codex",
		Version: "1.1.4",
		Distribution: agentcatalog.Distribution{NPX: &agentcatalog.PackageDistribution{
			Package: "@agentclientprotocol/codex-acp@1.1.4",
		}},
	}, nil)
	if err != nil {
		t.Fatalf("install from second catalog: %v", err)
	}

	installed, err := runningCatalog.Resolve("codex-acp")
	if err != nil {
		t.Fatalf("running catalog did not refresh: %v", err)
	}
	if installed.Command != "npx" {
		t.Errorf("refreshed command = %q, want npx", installed.Command)
	}
}

func TestCatalogInstallsBinaryAndResolvesItOffline(t *testing.T) {
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	contents := []byte("fake ACP Agent")
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "bin/fake-agent",
		Mode: 0o755,
		Size: int64(len(contents)),
	}); err != nil {
		t.Fatalf("write archive header: %v", err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatalf("write archive contents: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar archive: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip archive: %v", err)
	}
	digest := sha256.Sum256(archive.Bytes())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive.Bytes())
	}))
	platform, err := agentcatalog.CurrentPlatform()
	if err != nil {
		t.Fatalf("CurrentPlatform: %v", err)
	}
	dataDir := t.TempDir()
	catalogPath := filepath.Join(dataDir, "agents.json")
	agentsDir := filepath.Join(dataDir, "agents")
	catalog, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = catalog.Install(t.Context(), agentcatalog.RegistryAgent{
		ID:          "fake-agent",
		Name:        "Fake Agent",
		Version:     "1.0.0",
		Description: "A binary test Agent",
		Distribution: agentcatalog.Distribution{Binary: map[string]agentcatalog.BinaryDistribution{
			platform: {
				Archive: server.URL + "/fake-agent.tar.gz",
				SHA256:  fmt.Sprintf("%x", digest),
				Command: "./bin/fake-agent",
				Args:    []string{"acp"},
			},
		}},
	}, server.Client())
	if err != nil {
		t.Fatalf("Install binary: %v", err)
	}
	server.Close()

	reopened, err := agentcatalog.Open(catalogPath, agentsDir)
	if err != nil {
		t.Fatalf("reopen offline catalog: %v", err)
	}
	installed, err := reopened.Resolve("fake-agent")
	if err != nil {
		t.Fatalf("Resolve offline binary: %v", err)
	}
	wantCommand := filepath.Join(agentsDir, "fake-agent", "bin", "fake-agent")
	if installed.Command != wantCommand || installed.Type != agentcatalog.Binary {
		t.Errorf("installed binary = %#v, want command %q", installed, wantCommand)
	}
	gotContents, err := os.ReadFile(installed.Command)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(gotContents) != string(contents) {
		t.Errorf("installed binary contents = %q, want %q", gotContents, contents)
	}
	info, err := os.Stat(installed.Command)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode = %o, want executable", info.Mode().Perm())
	}
}
