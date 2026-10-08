package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codeasy-digix/codex-accounts/internal/historysync"
)

type cliFakeNative struct{ bundle *historysync.Bundle }

func (n *cliFakeNative) Inventory(context.Context) (map[string]historysync.Entry, error) {
	return map[string]historysync.Entry{n.bundle.Entry.ID: n.bundle.Entry}, nil
}
func (n *cliFakeNative) Export(context.Context, string) (*historysync.Bundle, error) {
	return n.bundle, nil
}
func (n *cliFakeNative) Install(_ context.Context, b *historysync.Bundle) (historysync.InstallResult, error) {
	return historysync.InstallResult{ID: b.Entry.ID, Status: "deferred"}, nil
}

func TestStatusAndConflictsShowDurableLedgerWithoutNativeAccess(t *testing.T) {
	root := t.TempDir()
	hub := filepath.Join(root, "hub")
	id := "01a0f156-d4b2-72a2-8a45-9834150686c9"
	for _, node := range []string{"a", "b"} {
		timestamp := "2026-10-01T00:00:01Z"
		message := "a"
		if node == "b" {
			timestamp = "2026-10-01T00:00:02Z"
			message = "b"
		}
		raw := []byte("{\"type\":\"session_meta\",\"timestamp\":\"2026-10-01T00:00:00Z\",\"payload\":{\"id\":\"" + id + "\"}}\n{\"type\":\"event_msg\",\"timestamp\":\"" + timestamp + "\",\"payload\":{\"type\":\"user_message\",\"message\":\"" + message + "\"}}\n")
		digest := sha256.Sum256(raw)
		last, _ := time.Parse(time.RFC3339Nano, timestamp)
		b := &historysync.Bundle{Format: historysync.Format, Node: node, Entry: historysync.Entry{ID: id, RolloutID: id, Digest: hex.EncodeToString(digest[:]), LastEventNS: last.UnixNano()}, Raw: raw, Row: map[string]any{"id": id, "history_mode": "legacy", "archived": json.Number("0")}, History: map[string][]map[string]any{}}
		relay, err := historysync.NewRelay(historysync.Config{Node: node, Home: filepath.Join(root, node, "native"), Store: filepath.Join(root, node, "store"), HubStore: hub, Enabled: true}, &cliFakeNative{bundle: b})
		if err != nil {
			t.Fatal(err)
		}
		if report, err := relay.Sync(context.Background(), false); err != nil {
			t.Fatalf("seed: %v %+v", err, report)
		}
	}
	cfg := historysync.Config{Node: "observer", Home: filepath.Join(root, "native-does-not-exist"), Store: filepath.Join(root, "observer"), HubStore: hub, Enabled: false}
	data, _ := json.Marshal(cfg)
	config := filepath.Join(root, "config.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config", config, "conflicts"}, bytes.NewReader(nil), &out); err != nil {
		t.Fatal(err)
	}
	var state historysync.HubState
	if err := json.Unmarshal(out.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ConflictsTotal != 1 || len(state.Conflicts) != 1 || !state.Conflicts[0].Resolved {
		t.Fatalf("durable ledger hidden: %s", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"status", "--config", config}, bytes.NewReader(nil), &out); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Hub     historysync.HubState `json:"hub"`
		Enabled bool                 `json:"enabled"`
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Hub.ConflictsTotal != 1 || status.Enabled {
		t.Fatal("status does not show durable conflict total", out.String())
	}
	if _, err := os.Stat(cfg.Home); !os.IsNotExist(err) {
		t.Fatal("status accessed native state", err)
	}
}

func TestServeNeedsNoConfigurationAndRejectsPartialMessage(t *testing.T) {
	var out bytes.Buffer
	store := filepath.Join(t.TempDir(), "hub")
	if err := run(context.Background(), []string{"--config", "/missing/config", "serve", "--store", store}, bytes.NewBufferString("{\"op\":\"heads\"}\n"), &out); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"serve", "--store", store}, bytes.NewBufferString("{\"op\":\"heads\"}"), &out); err == nil {
		t.Fatal("partial request accepted")
	}
}
