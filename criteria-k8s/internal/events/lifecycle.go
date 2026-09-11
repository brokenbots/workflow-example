package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Lifecycle event kinds emitted by the engine for per-scope adapter
// provisioning.
const (
	EventProvisionWanted = "provision_wanted"
	EventRelease         = "release"
)

// LifecycleEvent describes a provision-wanted or release event in the run
// event stream. The engine emits these at subworkflow scope entry and exit.
type LifecycleEvent struct {
	// Event is one of "provision_wanted" or "release".
	Event string `json:"event"`

	// RunID identifies the CriteriaRun the event belongs to.
	RunID string `json:"run_id"`

	// ScopeID is a unique identifier for the scope instance (root or a
	// named subworkflow + UUID).
	ScopeID string `json:"scope_id"`

	// ScopeTag is a short human-readable scope tag used in the handshake.
	ScopeTag string `json:"scope_tag,omitempty"`

	// AdapterName is the adapter kind (e.g. shell or copilot).
	AdapterName string `json:"adapter_name"`

	// Digest is the pinned lockfile digest for the adapter image, including
	// the "sha256:" prefix.
	Digest string `json:"digest,omitempty"`

	// ShimAddress is the runner dial address the adapter should phone home to.
	ShimAddress string `json:"shim_address,omitempty"`

	// TokenFile is an absolute path under /data to the per-scope bearer token.
	// Only present on provision-wanted events.
	TokenFile string `json:"token_file,omitempty"`

	// Timestamp is an optional RFC3339 event timestamp.
	Timestamp string `json:"timestamp,omitempty"`
}

// IsProvisionWanted returns true for a provision-wanted event.
func (e LifecycleEvent) IsProvisionWanted() bool {
	return e.Event == EventProvisionWanted
}

// IsRelease returns true for a release event.
func (e LifecycleEvent) IsRelease() bool {
	return e.Event == EventRelease
}

// ScopeKey returns a stable key combining the adapter kind and scope id.
func (e LifecycleEvent) ScopeKey() string {
	return e.AdapterName + "/" + e.ScopeID
}

// ParseLifecycleEvents scans an ndjson event stream and returns all lifecycle
// events in stream order. Non-JSON lines and unrelated events are ignored.
func ParseLifecycleEvents(r io.Reader) ([]LifecycleEvent, error) {
	var events []LifecycleEvent
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev LifecycleEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Event != EventProvisionWanted && ev.Event != EventRelease {
			continue
		}
		if ev.AdapterName == "" {
			continue
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning lifecycle events: %w", err)
	}
	return events, nil
}

// ParseLifecycleEventsBytes is a convenience wrapper around ParseLifecycleEvents.
func ParseLifecycleEventsBytes(data []byte) ([]LifecycleEvent, error) {
	return ParseLifecycleEvents(bytes.NewReader(data))
}

// ActiveProvisions returns the provision-wanted events that have not been
// released by a later release event for the same adapter/scope pair. The
// result is re-derivable from the full event stream and therefore idempotent.
func ActiveProvisions(events []LifecycleEvent) []LifecycleEvent {
	active := make(map[string]LifecycleEvent)
	for _, ev := range events {
		key := ev.ScopeKey()
		switch ev.Event {
		case EventProvisionWanted:
			active[key] = ev
		case EventRelease:
			delete(active, key)
		}
	}
	out := make([]LifecycleEvent, 0, len(active))
	for _, ev := range active {
		out = append(out, ev)
	}
	return out
}
