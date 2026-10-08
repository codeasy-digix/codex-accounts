// Package historysync implements an optional, separately installed history relay.
// It never copies credentials, GUI catalogs, or GUI connection settings.
package historysync

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

const Format = 2

type Entry struct {
	ID               string `json:"id"`
	Digest           string `json:"digest,omitempty"`
	LastEventNS      int64  `json:"last_event_ns,omitempty"`
	Archived         bool   `json:"archived"`
	Busy             bool   `json:"busy,omitempty"`
	Error            string `json:"error,omitempty"`
	RolloutID        string `json:"rollout_id,omitempty"`
	DependencyReason string `json:"dependency_reason,omitempty"`
}

type Bundle struct {
	Format     int                         `json:"format"`
	Node       string                      `json:"node"`
	SourceHome string                      `json:"source_home"`
	Entry      Entry                       `json:"entry"`
	Raw        []byte                      `json:"raw"`
	Row        map[string]any              `json:"row"`
	History    map[string][]map[string]any `json:"history"`
	Tools      []map[string]any            `json:"tools"`
}

type Config struct {
	Node            string `json:"node"`
	Home            string `json:"home"`
	Store           string `json:"store"`
	HubSSH          string `json:"hub_ssh,omitempty"`
	HubStore        string `json:"hub_store,omitempty"`
	HubBinary       string `json:"hub_binary,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	Enabled         bool   `json:"enabled"`
}

type InstallResult struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	PreviousDigest string `json:"previous_digest,omitempty"`
	Digest         string `json:"digest,omitempty"`
	RolloutID      string `json:"rollout_id,omitempty"`
}

// Native is the per-device adapter. The native writer protocol protects each
// export/publication. Import must prepare an immutable rollout generation and
// its projection before CAS-changing the stable thread's path in state.sqlite.
type Native interface {
	Inventory(context.Context) (map[string]Entry, error)
	Export(context.Context, string) (*Bundle, error)
	Install(context.Context, *Bundle) (InstallResult, error)
}

type CycleReport struct {
	Started         time.Time       `json:"started"`
	Finished        time.Time       `json:"finished"`
	Node            string          `json:"node"`
	Uploaded        int             `json:"uploaded"`
	Downloaded      int             `json:"downloaded"`
	Unchanged       int             `json:"unchanged"`
	Busy            int             `json:"busy"`
	Deferred        int             `json:"deferred"`
	Conflicts       int             `json:"conflicts"`
	ConflictsTotal  int             `json:"conflicts_total"`
	HubStateCurrent bool            `json:"hub_state_current"`
	Interrupted     bool            `json:"interrupted,omitempty"`
	Pending         int             `json:"pending"`
	Installed       int             `json:"installed"`
	Cached          int             `json:"cached"`
	SkippedLocal    int             `json:"skipped_local"`
	Errors          []string        `json:"errors,omitempty"`
	Results         []InstallResult `json:"results,omitempty"`
}

// Decode with UseNumber so SQLite integers and nanosecond timestamps never
// round-trip through float64.
func decodeJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(target)
}
