// Package agentcatalog owns the durable catalog of installed ACP Agents and
// the launch definitions used to spawn them.
package agentcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/aesoteric/aethos/internal/agent"
)

const catalogVersion = 1

var agentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// DistributionType identifies how an Agent is obtained and launched.
type DistributionType string

const (
	// NPX launches an npm package through npx.
	NPX DistributionType = "npx"
	// Binary launches an executable installed under the aethos data directory.
	Binary DistributionType = "binary"
	// UVX identifies a Python package distribution that aethos does not install.
	UVX DistributionType = "uvx"
)

// PackageDistribution describes an npx registry distribution.
type PackageDistribution struct {
	Package string            `json:"package"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// BinaryDistribution describes one platform-specific binary archive.
type BinaryDistribution struct {
	Archive string            `json:"archive"`
	SHA256  string            `json:"sha256,omitempty"`
	Command string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Distribution contains every installation method published for one Agent.
type Distribution struct {
	NPX    *PackageDistribution          `json:"npx,omitempty"`
	UVX    *PackageDistribution          `json:"uvx,omitempty"`
	Binary map[string]BinaryDistribution `json:"binary,omitempty"`
}

// RegistryAgent is one entry returned by the ACP Agent registry.
type RegistryAgent struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Distribution Distribution `json:"distribution"`
}

// InstalledAgent is a durable, directly launchable catalog entry.
type InstalledAgent struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description"`
	Type        DistributionType `json:"type"`
	agent.Launch
}

// InstalledLister supplies the current Agent choices to setup and Channels.
type InstalledLister interface {
	Installed() ([]InstalledAgent, error)
}

type catalogFile struct {
	Version int                       `json:"version"`
	Agents  map[string]InstalledAgent `json:"agents"`
}

// Catalog persists installed Agent launch definitions in one JSON file.
type Catalog struct {
	mu        sync.Mutex
	path      string
	agentsDir string
	agents    map[string]InstalledAgent
}

// Open loads an Agent catalog, treating a missing file as an empty catalog.
func Open(path, agentsDir string) (*Catalog, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("agent catalog path must be absolute, got %q", path)
	}
	if !filepath.IsAbs(agentsDir) {
		return nil, fmt.Errorf("agent installation directory must be absolute, got %q", agentsDir)
	}
	catalog := &Catalog{
		path:      filepath.Clean(path),
		agentsDir: filepath.Clean(agentsDir),
		agents:    make(map[string]InstalledAgent),
	}
	if err := catalog.refreshLocked(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// Install records one registry Agent. Binary distributions are downloaded by
// the supplied client; npx distributions need no eager download.
func (c *Catalog) Install(ctx context.Context, registryAgent RegistryAgent, client *http.Client) (InstalledAgent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshLocked(); err != nil {
		return InstalledAgent{}, err
	}
	if err := ctx.Err(); err != nil {
		return InstalledAgent{}, err
	}
	id := strings.TrimSpace(registryAgent.ID)
	if !agentIDPattern.MatchString(id) {
		return InstalledAgent{}, fmt.Errorf("install Agent: invalid registry ID %q", id)
	}
	if _, exists := c.agents[id]; exists {
		return InstalledAgent{}, fmt.Errorf("install Agent %q: already installed", id)
	}
	installed := InstalledAgent{
		ID: id, Name: registryAgent.Name, Version: registryAgent.Version,
		Description: registryAgent.Description,
	}
	removeBinaryOnFailure := false
	switch registryAgent.Type() {
	case NPX:
		packageName := strings.TrimSpace(registryAgent.Distribution.NPX.Package)
		if packageName == "" {
			return InstalledAgent{}, fmt.Errorf("install Agent %q: npx package is required", id)
		}
		installed.Type = NPX
		installed.Command = "npx"
		installed.Args = append([]string{"--yes", packageName}, registryAgent.Distribution.NPX.Args...)
		installed.Env = cloneMap(registryAgent.Distribution.NPX.Env)
	case Binary:
		platform, err := CurrentPlatform()
		if err != nil {
			return InstalledAgent{}, fmt.Errorf("install Agent %q: %w", id, err)
		}
		distribution, ok := registryAgent.Distribution.Binary[platform]
		if !ok {
			return InstalledAgent{}, fmt.Errorf("install Agent %q: binary is unavailable for %s", id, platform)
		}
		command, err := installBinary(ctx, client, c.agentsDir, id, distribution)
		if err != nil {
			return InstalledAgent{}, fmt.Errorf("install Agent %q: %w", id, err)
		}
		removeBinaryOnFailure = true
		installed.Type = Binary
		installed.Command = command
		installed.Args = append([]string(nil), distribution.Args...)
		installed.Env = cloneMap(distribution.Env)
	default:
		return InstalledAgent{}, fmt.Errorf("install Agent %q: no supported npx or binary distribution", id)
	}
	c.agents[id] = installed
	if err := c.persist(); err != nil {
		delete(c.agents, id)
		if removeBinaryOnFailure {
			_ = os.RemoveAll(filepath.Join(c.agentsDir, id))
		}
		return InstalledAgent{}, err
	}
	return installed, nil
}

// Resolve returns the launch definition for an installed Agent ID.
func (c *Catalog) Resolve(id string) (InstalledAgent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshLocked(); err != nil {
		return InstalledAgent{}, err
	}
	installed, ok := c.agents[strings.TrimSpace(id)]
	if !ok {
		return InstalledAgent{}, fmt.Errorf("agent %q is not installed", id)
	}
	return cloneInstalledAgent(installed), nil
}

// Installed returns every installed Agent in stable ID order.
func (c *Catalog) Installed() ([]InstalledAgent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshLocked(); err != nil {
		return nil, err
	}
	installed := make([]InstalledAgent, 0, len(c.agents))
	for _, agent := range c.agents {
		installed = append(installed, cloneInstalledAgent(agent))
	}
	sort.Slice(installed, func(i, j int) bool { return installed[i].ID < installed[j].ID })
	return installed, nil
}

func (c *Catalog) refreshLocked() error {
	contents, err := os.ReadFile(c.path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		c.agents = make(map[string]InstalledAgent)
		return nil
	default:
		return fmt.Errorf("read Agent catalog %q: %w", c.path, err)
	}
	var persisted catalogFile
	if err := json.Unmarshal(contents, &persisted); err != nil {
		return fmt.Errorf("parse Agent catalog %q: %w", c.path, err)
	}
	if persisted.Version != catalogVersion {
		return fmt.Errorf("agent catalog %q has unsupported version %d", c.path, persisted.Version)
	}
	if persisted.Agents == nil {
		persisted.Agents = make(map[string]InstalledAgent)
	}
	c.agents = persisted.Agents
	return nil
}

func (c *Catalog) persist() (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create Agent catalog directory: %w", err)
	}
	contents, err := json.MarshalIndent(catalogFile{Version: catalogVersion, Agents: c.agents}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Agent catalog: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(c.path), ".agents-*.json")
	if err != nil {
		return fmt.Errorf("create temporary Agent catalog: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary Agent catalog: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write temporary Agent catalog: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary Agent catalog: %w", err)
	}
	closeErr := temporary.Close()
	closed = true
	if closeErr != nil {
		return fmt.Errorf("close temporary Agent catalog: %w", closeErr)
	}
	if err := os.Rename(temporaryPath, c.path); err != nil {
		return fmt.Errorf("replace Agent catalog %q: %w", c.path, err)
	}
	return nil
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneInstalledAgent(installed InstalledAgent) InstalledAgent {
	installed.Launch = installed.Clone()
	return installed
}
