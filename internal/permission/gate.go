// Package permission implements the compiled-in permission gate Module.
package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
)

// DefaultTimeout bounds how long a permission request can pause a Prompt.
const DefaultTimeout = 10 * time.Minute

// ErrUnknownRequest reports a response for an identity this Gate never issued.
var ErrUnknownRequest = errors.New("unknown permission request")

// ErrUnknownOption reports an Agent option identity that was not offered on
// the permission request.
var ErrUnknownOption = errors.New("unknown permission option")

// Present surfaces a pending request to the Session's Channel.
type Present func(context.Context, string, agent.PermissionRequested) error

// Report records a failure that was converted into a fail-safe decision rather
// than returned to the Agent as an ACP error.
type Report func(error)

// Gate owns pending permission requests and their exactly-once resolution.
type Gate struct {
	timeout     time.Duration
	autoApprove []string
	present     Present
	report      Report

	mu       sync.Mutex
	pending  map[string]*pendingRequest
	idPrefix string
	issued   uint64
}

// Result contains the Agent-facing decision and whether Channels should apply
// a terminal update to any permission request they successfully presented.
type Result struct {
	RequestID     string
	Decision      agent.PermissionDecision
	NotifyOutcome bool
	TimedOut      bool
}

type pendingRequest struct {
	options []agent.PermissionOption
	done    chan Result
}

// New constructs a Gate. autoApprove contains exact Agent tool kinds, such as
// "read" or "search", that may select their first allow option silently.
func New(timeout time.Duration, autoApprove []string, present Present, report Report) (*Gate, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("permission timeout must be greater than zero")
	}
	if present == nil {
		return nil, fmt.Errorf("permission presenter is required")
	}
	idPrefix, err := newIDPrefix()
	if err != nil {
		return nil, fmt.Errorf("create permission request identity: %w", err)
	}
	return &Gate{
		timeout:     timeout,
		autoApprove: append([]string(nil), autoApprove...),
		present:     present,
		report:      report,
		pending:     make(map[string]*pendingRequest),
		idPrefix:    idPrefix,
	}, nil
}

// Request applies auto-approve rules or pauses until the Channel resolves the
// request. Context cancellation cancels the ACP request rather than selecting
// an option the user did not choose.
func (g *Gate) Request(ctx context.Context, sessionID string, request agent.PermissionRequest) (Result, error) {
	if slices.Contains(g.autoApprove, request.Kind) {
		if option, ok := optionByDisposition(request.Options, true); ok {
			return Result{Decision: agent.PermissionDecision{OptionID: option.ID}}, nil
		}
	}

	pending := &pendingRequest{
		options: append([]agent.PermissionOption(nil), request.Options...),
		done:    make(chan Result, 1),
	}
	g.mu.Lock()
	g.issued++
	id := fmt.Sprintf("%s-%x", g.idPrefix, g.issued)
	g.pending[id] = pending
	g.mu.Unlock()

	requested := agent.PermissionRequested{ID: id, PermissionRequest: request}
	requested.Options = append([]agent.PermissionOption(nil), request.Options...)
	timer := time.NewTimer(g.timeout)
	defer timer.Stop()
	presentCtx, cancelPresent := context.WithCancel(ctx)
	defer cancelPresent()
	presented := make(chan error, 1)
	go func() {
		presented <- g.present(presentCtx, sessionID, requested)
	}()

	for {
		select {
		case result := <-pending.done:
			return result, nil
		case err := <-presented:
			if err != nil {
				g.reportError(fmt.Errorf("present permission request %q for Session %q: %w", id, sessionID, err))
				g.finish(id, Result{Decision: failSafeDecision(request.Options)})
				return <-pending.done, nil
			}
			presented = nil
		case <-timer.C:
			g.finish(id, Result{
				Decision:      failSafeDecision(request.Options),
				NotifyOutcome: true,
				TimedOut:      true,
			})
			return <-pending.done, nil
		case <-ctx.Done():
			g.finish(id, Result{
				Decision:      agent.PermissionDecision{Cancelled: true},
				NotifyOutcome: true,
			})
			return <-pending.done, nil
		}
	}
}

// Resolve selects one of the Agent-provided options. Repeating a completed
// resolution is harmless, including when two Channels race to answer.
func (g *Gate) Resolve(requestID, optionID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.pending[requestID]
	if !ok {
		if g.wasIssuedLocked(requestID) {
			return nil
		}
		return fmt.Errorf("resolve permission %q: %w", requestID, ErrUnknownRequest)
	}
	if !slices.ContainsFunc(pending.options, func(option agent.PermissionOption) bool {
		return option.ID == optionID
	}) {
		return fmt.Errorf("resolve permission %q with option %q: %w", requestID, optionID, ErrUnknownOption)
	}
	g.finishLocked(requestID, pending, Result{
		Decision:      agent.PermissionDecision{OptionID: optionID},
		NotifyOutcome: true,
	})
	return nil
}

func (g *Gate) finish(requestID string, result Result) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.pending[requestID]
	if !ok {
		return
	}
	g.finishLocked(requestID, pending, result)
}

func (g *Gate) finishLocked(requestID string, pending *pendingRequest, result Result) {
	delete(g.pending, requestID)
	result.RequestID = requestID
	pending.done <- result
}

func (g *Gate) wasIssuedLocked(requestID string) bool {
	sequence, ok := strings.CutPrefix(requestID, g.idPrefix+"-")
	if !ok {
		return false
	}
	number, err := strconv.ParseUint(sequence, 16, 64)
	return err == nil && number > 0 && number <= g.issued
}

func (g *Gate) reportError(err error) {
	if g.report != nil {
		g.report(err)
	}
}

func failSafeDecision(options []agent.PermissionOption) agent.PermissionDecision {
	if option, ok := optionByDisposition(options, false); ok {
		return agent.PermissionDecision{OptionID: option.ID}
	}
	return agent.PermissionDecision{Cancelled: true}
}

func optionByDisposition(options []agent.PermissionOption, allow bool) (agent.PermissionOption, bool) {
	once := agent.PermissionRejectOnce
	always := agent.PermissionRejectAlways
	if allow {
		once = agent.PermissionAllowOnce
		always = agent.PermissionAllowAlways
	}
	var fallback agent.PermissionOption
	for _, option := range options {
		if option.Kind == once {
			return option, true
		}
		if option.Kind == always {
			fallback = option
		}
	}
	if fallback.ID != "" {
		return fallback, true
	}
	return agent.PermissionOption{}, false
}

func newIDPrefix() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
