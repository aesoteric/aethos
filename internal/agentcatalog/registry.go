package agentcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultRegistryURL is the official ACP Agent registry index.
const DefaultRegistryURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

const maxRegistryBytes = 16 << 20

// Unsupported marks a registry distribution that aethos cannot install.
const Unsupported DistributionType = "unsupported"

// Registry fetches Agent metadata from the ACP registry protocol edge.
type Registry struct {
	endpoint string
	client   *http.Client
}

// NewRegistry creates an ACP registry client. Empty values select the official
// registry endpoint and the default HTTP client.
func NewRegistry(endpoint string, client *http.Client) *Registry {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultRegistryURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Registry{endpoint: endpoint, client: client}
}

// List fetches the current registry Agent list.
func (r *Registry) List(ctx context.Context) (_ []RegistryAgent, returnErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch ACP Agent registry: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch ACP Agent registry: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, annotate("close ACP Agent registry response", response.Body.Close()))
	}()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("fetch ACP Agent registry: unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxRegistryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch ACP Agent registry: read response: %w", err)
	}
	if len(contents) > maxRegistryBytes {
		return nil, fmt.Errorf("fetch ACP Agent registry: response exceeds %d bytes", maxRegistryBytes)
	}
	var index struct {
		Version string          `json:"version"`
		Agents  []RegistryAgent `json:"agents"`
	}
	if err := json.Unmarshal(contents, &index); err != nil {
		return nil, fmt.Errorf("fetch ACP Agent registry: decode response: %w", err)
	}
	if strings.TrimSpace(index.Version) == "" {
		return nil, fmt.Errorf("fetch ACP Agent registry: response version is required")
	}
	return index.Agents, nil
}

// Type reports the preferred distribution aethos can install for this Agent.
func (a RegistryAgent) Type() DistributionType {
	if a.Distribution.NPX != nil {
		return NPX
	}
	if len(a.Distribution.Binary) > 0 {
		return Binary
	}
	if a.Distribution.UVX != nil {
		return UVX
	}
	return Unsupported
}

func annotate(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
