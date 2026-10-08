package historysync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureThreadID = "11111111-1111-4111-8111-111111111111"

// These are native 0.162 schemas, rather than generated from adapter allowlists.
const fixtureStateSchema = `
PRAGMA journal_mode=WAL;
CREATE TABLE threads (
 id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL, source TEXT NOT NULL, model_provider TEXT NOT NULL,
 cwd TEXT NOT NULL, title TEXT NOT NULL, sandbox_policy TEXT NOT NULL,
 approval_mode TEXT NOT NULL, tokens_used INTEGER NOT NULL DEFAULT 0,
 has_user_event INTEGER NOT NULL DEFAULT 0, archived INTEGER NOT NULL DEFAULT 0,
 archived_at INTEGER, git_sha TEXT, git_branch TEXT, git_origin_url TEXT,
 cli_version TEXT NOT NULL DEFAULT '', first_user_message TEXT NOT NULL DEFAULT '',
 agent_nickname TEXT, agent_role TEXT, memory_mode TEXT NOT NULL DEFAULT 'enabled',
 model TEXT, reasoning_effort TEXT, agent_path TEXT, created_at_ms INTEGER,
 updated_at_ms INTEGER, thread_source TEXT, preview TEXT NOT NULL DEFAULT '',
 recency_at INTEGER NOT NULL DEFAULT 0, recency_at_ms INTEGER NOT NULL DEFAULT 0,
 history_mode TEXT NOT NULL DEFAULT 'legacy', name TEXT, is_pinned INTEGER NOT NULL DEFAULT 0,
 thread_section_id TEXT, section_position INTEGER, section_entered_at_ms INTEGER,
 project_id TEXT, originator TEXT, daybreak_enabled BOOLEAN, creator_user_id TEXT,
 creator_account_id TEXT
);
CREATE TABLE thread_dynamic_tools (
 thread_id TEXT NOT NULL, position INTEGER NOT NULL, name TEXT NOT NULL,
 description TEXT NOT NULL, input_schema TEXT NOT NULL, defer_loading INTEGER NOT NULL DEFAULT 0,
 namespace TEXT, PRIMARY KEY(thread_id,position)
);
CREATE TABLE thread_attachments (
 id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, attachment_type TEXT NOT NULL,
 identity_key TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE backfill_state (id INTEGER PRIMARY KEY CHECK(id=1),status TEXT NOT NULL,
 last_watermark TEXT,last_success_at INTEGER,updated_at INTEGER NOT NULL);
INSERT INTO backfill_state VALUES (1,'complete',NULL,1,1);`

const fixtureHistorySchema = `
PRAGMA journal_mode=WAL;
CREATE TABLE thread_history_projection_state (thread_id TEXT PRIMARY KEY,
 next_rollout_byte_offset INTEGER NOT NULL,next_rollout_ordinal INTEGER NOT NULL);
CREATE TABLE thread_turns (thread_id TEXT NOT NULL,turn_id TEXT NOT NULL,
 rollout_ordinal INTEGER NOT NULL,status TEXT NOT NULL,error_json TEXT,started_at INTEGER,
 completed_at INTEGER,duration_ms INTEGER,first_user_item_id TEXT,final_agent_item_id TEXT,
 rollout_byte_offset INTEGER,rollout_end_ordinal INTEGER,rollout_end_byte_offset INTEGER,
 PRIMARY KEY(thread_id,turn_id));
CREATE TABLE thread_items (thread_id TEXT NOT NULL,turn_id TEXT NOT NULL,item_id TEXT NOT NULL,
 rollout_ordinal INTEGER NOT NULL,created_at_ms INTEGER NOT NULL,item_json TEXT NOT NULL,
 item_type TEXT NOT NULL DEFAULT '',updated_at_ordinal INTEGER NOT NULL DEFAULT 0,
 started_at_ms INTEGER,completed_at_ms INTEGER,PRIMARY KEY(thread_id,turn_id,item_id));
CREATE TABLE thread_realtime_items (thread_id TEXT NOT NULL,item_id TEXT NOT NULL,
 rollout_ordinal INTEGER NOT NULL,created_at_ms INTEGER NOT NULL,item_type TEXT NOT NULL,
 item_json TEXT NOT NULL,PRIMARY KEY(thread_id,item_id));`

func fixtureNative(t *testing.T) *nativeAdapter {
	t.Helper()
	return fixtureNativeAt(t, t.TempDir())
}

func fixtureNativeAt(t *testing.T, root string) *nativeAdapter {
	t.Helper()
	home := filepath.Join(root, "user", ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	for file, schema := range map[string]string{"state_5.sqlite": fixtureStateSchema, "thread_history_1.sqlite": fixtureHistorySchema} {
		db, err := sql.Open("sqlite", filepath.Join(home, file))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
	}
	adapter, err := NewNative(Config{Node: "fixture", Home: home, Store: filepath.Join(root, "relay")})
	if err != nil {
		t.Fatal(err)
	}
	n := adapter.(*nativeAdapter)
	n.processGuard = func(context.Context) (bool, error) { return false, nil }
	return n
}

func TestNativeSnapshotPublication(t *testing.T) {
	source, id, output := os.Getenv("HISTORY_SYNC_FIXTURE_SOURCE"), os.Getenv("HISTORY_SYNC_FIXTURE_THREAD"), os.Getenv("HISTORY_SYNC_FIXTURE_OUTPUT")
	if source == "" || id == "" || output == "" {
		t.Skip("optional official-reader fixture")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture output must be a new path")
	}
	src, err := NewNative(Config{Node: "snapshot", Home: source, Store: filepath.Join(t.TempDir(), "relay")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := src.Export(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Entry.DependencyReason != "" {
		t.Fatalf("snapshot cannot be published: %s", b.Entry.DependencyReason)
	}
	n := fixtureNativeAt(t, output)
	r, err := n.Install(context.Background(), b)
	if err == nil && r.Status == "deferred" && strings.Contains(r.Reason, "permissions") {
		t.Logf("snapshot archived; immutable publication safely deferred: %s", r.Reason)
		return
	}
	if err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	t.Logf("published native fixture home=%s thread=%s rollout=%s", n.cfg.Home, id, r.RolloutID)
}

func TestNativeArchivesDependenciesAndDetectsProjectionRepair(t *testing.T) {
	n := fixtureNative(t)
	ctx := context.Background()
	b := fixtureBundle(t, "paginated", 0)
	if r, err := n.Install(ctx, b); err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	db, _, err := n.state(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO thread_attachments VALUES ('attachment',?,'worktree','key','{}',1)", fixtureThreadID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	exported, err := n.Export(ctx, fixtureThreadID)
	if err != nil || exported.Entry.DependencyReason == "" || !bytes.Equal(exported.Raw, b.Raw) {
		t.Fatalf("attachment raw archival failed: %v", err)
	}
	target := fixtureNative(t)
	r, err := target.Install(ctx, exported)
	if err != nil || r.Status != "deferred" {
		t.Fatalf("%+v %v", r, err)
	}
	db, _, err = n.state(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM thread_attachments"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	row := fixtureReadRow(t, n)
	rolloutID, err := nativeRolloutID(row["rollout_path"].(string), fixtureThreadID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := openNativeDB(filepath.Join(n.cfg.Home, "thread_history_1.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecContext(ctx, "UPDATE thread_history_projection_state SET next_rollout_byte_offset=0 WHERE thread_id=?", rolloutID); err != nil {
		t.Fatal(err)
	}
	inv, err := n.Inventory(ctx)
	if err != nil || inv[fixtureThreadID].DependencyReason == "" {
		t.Fatal("lagging projection was presented as healthy")
	}
	exported, err = n.Export(ctx, fixtureThreadID)
	if err != nil || exported.Entry.DependencyReason == "" || exported.Entry.Digest != b.Entry.Digest {
		t.Fatal("lagging projection prevented raw archival")
	}
	if _, err := h.ExecContext(ctx, "UPDATE thread_history_projection_state SET next_rollout_byte_offset=? WHERE thread_id=?", len(b.Raw), rolloutID); err != nil {
		t.Fatal(err)
	}
	h.Close()
	inv, err = n.Inventory(ctx)
	if err != nil || inv[fixtureThreadID].DependencyReason != "" {
		t.Fatalf("mutable projection repair stayed cached: %v %+v", err, inv)
	}
}

func TestNativeInventoryFingerprintSurvivesPreservedMtimeRestore(t *testing.T) {
	n := fixtureNative(t)
	ctx := context.Background()
	b := fixtureBundle(t, "legacy", 0)
	if r, err := n.Install(ctx, b); err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	before, err := n.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row := fixtureReadRow(t, n)
	filename := row["rollout_path"].(string)
	stat, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	restored := fixtureBundle(t, "legacy", time.Second)
	if len(restored.Raw) != len(b.Raw) {
		t.Fatal("restore fixture size differs")
	}
	temp := filename + ".restored"
	if err := os.WriteFile(temp, restored.Raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temp, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, filename); err != nil {
		t.Fatal(err)
	}
	after, err := n.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after[fixtureThreadID].Digest == before[fixtureThreadID].Digest || after[fixtureThreadID].Digest != restored.Entry.Digest {
		t.Fatal("same-size restored rollout reused a stale fingerprint")
	}
}

func fixtureBundle(t *testing.T, mode string, later time.Duration) *Bundle {
	t.Helper()
	ts := time.Date(2026, 10, 9, 0, 0, 0, 123456789, time.UTC).Add(later).Format(time.RFC3339Nano)
	records := []map[string]any{
		{"type": "session_meta", "timestamp": ts, "ordinal": int64(0), "payload": map[string]any{"id": fixtureThreadID, "history_mode": mode}},
		{"type": "event_msg", "timestamp": ts, "ordinal": int64(1), "payload": map[string]any{"type": "task_started", "turn_id": "turn-1"}},
		{"type": "turn_context", "timestamp": ts, "ordinal": int64(2), "payload": map[string]any{"turn_id": "turn-1", "sandbox_policy": map[string]any{"type": "read-only"}, "approval_policy": "on-request"}},
		{"type": "response_item", "timestamp": ts, "ordinal": int64(3), "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}}},
		// Native permits sparse ordinals; byte positions still refer to physical lines.
		{"type": "response_item", "timestamp": ts, "ordinal": int64(5), "payload": map[string]any{"type": "agent_message", "text": "answer"}},
		{"type": "world_state", "timestamp": ts, "ordinal": int64(6), "payload": map[string]any{}},
		{"type": "event_msg", "timestamp": ts, "ordinal": int64(7), "payload": map[string]any{"type": "task_complete", "turn_id": "turn-1"}},
	}
	var raw []byte
	for _, record := range records {
		if mode == "legacy" {
			delete(record, "ordinal")
		}
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, line...)
		raw = append(raw, '\n')
	}
	info, err := inspectNativeRaw(raw, fixtureThreadID)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bundle{Format: Format, Node: "source", SourceHome: "/remote/.codex", Raw: raw,
		Entry:   Entry{ID: fixtureThreadID, Digest: info.digest, LastEventNS: info.lastNS, RolloutID: fixtureThreadID},
		Row:     map[string]any{"id": fixtureThreadID, "rollout_path": "/remote/.codex/sessions/rollout-2026-10-09T00-00-00-" + fixtureThreadID + ".jsonl", "created_at": info.lastNS / int64(time.Second), "updated_at": info.lastNS / int64(time.Second), "source": "cli", "model_provider": "openai", "cwd": "/remote/project", "title": "Fixture history", "sandbox_policy": `{"type":"danger-full-access"}`, "approval_mode": "never", "tokens_used": int64(9007199254740993), "has_user_event": int64(1), "archived": int64(0), "history_mode": mode, "is_pinned": int64(1), "memory_mode": "disabled", "name": "Foreign name", "daybreak_enabled": int64(1), "creator_account_id": "foreign-account", "project_id": "foreign-project"},
		History: make(map[string][]map[string]any),
		Tools:   []map[string]any{{"thread_id": fixtureThreadID, "position": int64(0), "name": "tool", "description": "test", "input_schema": `{"type":"object"}`, "defer_loading": int64(0), "namespace": nil}},
	}
	if mode == "paginated" {
		b.History["thread_history_projection_state"] = []map[string]any{{"thread_id": fixtureThreadID, "next_rollout_byte_offset": int64(len(raw)), "next_rollout_ordinal": int64(8)}}
		b.History["thread_turns"] = []map[string]any{{"thread_id": fixtureThreadID, "turn_id": "turn-1", "rollout_ordinal": int64(1), "status": "completed", "first_user_item_id": "user-1", "final_agent_item_id": "agent-1", "rollout_byte_offset": info.starts[1], "rollout_end_ordinal": int64(7), "rollout_end_byte_offset": info.ends[7]}}
		b.History["thread_items"] = []map[string]any{
			{"thread_id": fixtureThreadID, "turn_id": "turn-1", "item_id": "user-1", "rollout_ordinal": int64(3), "created_at_ms": info.lastNS / int64(time.Millisecond), "item_json": `{"type":"userMessage","id":"user-1"}`, "item_type": "userMessage", "updated_at_ordinal": int64(3)},
			{"thread_id": fixtureThreadID, "turn_id": "turn-1", "item_id": "agent-1", "rollout_ordinal": int64(5), "created_at_ms": info.lastNS / int64(time.Millisecond), "item_json": `{"type":"agentMessage","id":"agent-1"}`, "item_type": "agentMessage", "updated_at_ordinal": int64(5)},
		}
		b.History["thread_realtime_items"] = []map[string]any{}
	}
	return b
}

func fixtureReadRow(t *testing.T, n *nativeAdapter) map[string]any {
	t.Helper()
	db, _, err := n.state(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := nativeRows(context.Background(), db, "SELECT * FROM threads WHERE id = ?", fixtureThreadID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	return rows[0]
}

func TestNativeImmutableLifecycle(t *testing.T) {
	for _, mode := range []string{"paginated", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			n := fixtureNative(t)
			b := fixtureBundle(t, mode, 0)
			ctx := context.Background()
			result, err := n.Install(ctx, b)
			if err != nil || result.Status != "installed" {
				t.Fatalf("%+v %v", result, err)
			}
			row := fixtureReadRow(t, n)
			oldPath := row["rollout_path"].(string)
			if row["id"] != fixtureThreadID || result.RolloutID == fixtureThreadID || !strings.HasSuffix(oldPath, fixtureThreadID+"_"+result.RolloutID+".jsonl") {
				t.Fatalf("unexpected identity %+v", result)
			}
			if row["sandbox_policy"] != nativeReadOnlyProfile || row["approval_mode"] != "on-request" || row["memory_mode"] != "enabled" || row["is_pinned"] != int64(0) || row["creator_account_id"] != nil || row["project_id"] != nil || row["name"] != nil || row["daybreak_enabled"] != nil {
				t.Fatalf("foreign destination settings: %+v", row)
			}
			raw, err := os.ReadFile(oldPath)
			if err != nil || !bytes.Equal(raw, b.Raw) {
				t.Fatal("raw rollout was rewritten")
			}
			exported, err := n.Export(ctx, fixtureThreadID)
			if err != nil || exported.Entry.DependencyReason != "" || !bytes.Equal(exported.Raw, b.Raw) {
				t.Fatalf("export: %v %+v", err, exported)
			}
			wire, err := json.Marshal(exported)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Bundle
			if err := decodeJSON(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			integer, err := nativeNumber(decoded.Row["tokens_used"])
			if err != nil || integer != 9007199254740993 {
				t.Fatal("SQLite integer precision lost")
			}
			updated := fixtureBundle(t, mode, time.Second)
			result2, err := n.Install(ctx, updated)
			if err != nil || result2.Status != "installed" || result2.RolloutID == result.RolloutID {
				t.Fatalf("update: %+v %v", result2, err)
			}
			if original, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(original, b.Raw) {
				t.Fatal("previous rollout was modified")
			}
			db, _, err := n.state(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := nativeRows(ctx, db, "SELECT COUNT(*) AS count FROM threads")
			db.Close()
			if err != nil || rows[0]["count"] != int64(1) {
				t.Fatalf("new chat fork created: %v %v", rows, err)
			}
			if mode == "paginated" {
				h, err := openNativeDB(filepath.Join(n.cfg.Home, "thread_history_1.sqlite"), false)
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				for _, id := range []string{result.RolloutID, result2.RolloutID} {
					rows, err := nativeRows(ctx, h, "SELECT COUNT(*) AS count FROM thread_items WHERE thread_id = ?", id)
					if err != nil || rows[0]["count"] != int64(2) {
						t.Fatalf("projection generation missing %s", id)
					}
				}
			}
		})
	}
}

func TestNativePublicationCrashProtectsPreviousPointer(t *testing.T) {
	n := fixtureNative(t)
	ctx := context.Background()
	old := fixtureBundle(t, "paginated", 0)
	if r, err := n.Install(ctx, old); err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	oldRow := fixtureReadRow(t, n)
	n.publicationHook = func(phase string) error {
		if phase == "projection_ready" {
			return errors.New("simulated power loss")
		}
		return nil
	}
	if _, err := n.Install(ctx, fixtureBundle(t, "paginated", time.Second)); err == nil {
		t.Fatal("crash hook did not fire")
	}
	row := fixtureReadRow(t, n)
	if row["rollout_path"] != oldRow["rollout_path"] {
		t.Fatal("crash exposed incomplete pointer")
	}
	count := 0
	if err := filepath.Walk(filepath.Join(n.cfg.Home, "sessions"), func(filename string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(filename, ".jsonl") {
			count++
		}
		return nil
	}); err != nil || count != 1 {
		t.Fatal("uncommitted raw rollout was left discoverable")
	}
	export, err := n.Export(ctx, fixtureThreadID)
	if err != nil || !bytes.Equal(export.Raw, old.Raw) {
		t.Fatal("old conversation unreadable after crash")
	}
}

func TestNativeProcessDetection(t *testing.T) {
	for _, tc := range []struct {
		line string
		busy bool
	}{
		{"123 codex-history-s /usr/local/bin/codex-history-sync daemon", false},
		{"123 codex-history-sync /usr/local/bin/codex-history-sync hub-put", false},
		{"123 ChatGPT /Applications/ChatGPT.app/Contents/MacOS/ChatGPT", true},
		{"123 ChatGPTService /Applications/ChatGPT.app/Contents/Frameworks/ChatGPT Service", true},
		{"123 codex /usr/local/bin/codex app-server", true},
		{"123 Codex /Applications/Codex.app/Contents/MacOS/Codex", true},
		{"123 node node /tmp/ordinary.js", false},
		{"999 codex /bin/codex", false},
	} {
		busy, err := nativeProcessListingRunning([]byte(tc.line+"\n"), 999)
		if err != nil || busy != tc.busy {
			t.Fatalf("%q busy=%v err=%v", tc.line, busy, err)
		}
	}
}

func TestNativeDefersWithoutNativeChanges(t *testing.T) {
	for _, mode := range []string{"runtime", "dependency", "writer"} {
		t.Run(mode, func(t *testing.T) {
			n := fixtureNative(t)
			b := fixtureBundle(t, "paginated", 0)
			switch mode {
			case "runtime":
				n.processGuard = func(context.Context) (bool, error) { return true, nil }
			case "dependency":
				b.Entry.DependencyReason = "attachment requires separate transfer"
			case "writer":
				guard, err := acquireNativeWriter(n.cfg.Home, fixtureThreadID)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
			}
			r, err := n.Install(context.Background(), b)
			if err != nil || (r.Status != "deferred" && r.Status != "busy") {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err := os.Stat(filepath.Join(n.cfg.Home, "sessions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("deferred install created native history")
			}
			if mode == "runtime" {
				if _, err := os.Stat(filepath.Join(n.cfg.Store, "native-staging")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("active runtime caused a redundant native stage write")
				}
			}
			db, _, err := n.state(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := nativeRows(context.Background(), db, "SELECT COUNT(*) AS count FROM threads")
			if err != nil || rows[0]["count"] != int64(0) {
				t.Fatal("deferred install changed threads")
			}
		})
	}
}

func TestNativeRejectsIncompleteOrForgedHistory(t *testing.T) {
	for _, mode := range []string{"partial", "digest", "timestamp", "offset", "empty_projection", "missing_turn", "item_reference", "unknown_column"} {
		t.Run(mode, func(t *testing.T) {
			n := fixtureNative(t)
			b := fixtureBundle(t, "paginated", 0)
			switch mode {
			case "partial":
				b.Raw = b.Raw[:len(b.Raw)-1]
			case "digest":
				b.Entry.Digest = strings.Repeat("0", 64)
			case "timestamp":
				b.Entry.LastEventNS++
			case "offset":
				b.History["thread_turns"][0]["rollout_byte_offset"] = int64(2)
			case "empty_projection":
				b.History["thread_items"] = nil
			case "missing_turn":
				b.History["thread_turns"] = nil
			case "item_reference":
				b.History["thread_turns"][0]["first_user_item_id"] = "absent"
			case "unknown_column":
				b.Row["foreign_unsupported"] = "value"
			}
			r, err := n.Install(context.Background(), b)
			if err != nil || r.Status != "deferred" || r.Reason == "" {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err := os.Stat(filepath.Join(n.cfg.Home, "sessions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid bundle touched native history")
			}
		})
	}
}

func TestNativeExclusiveWALLockBlocksStartupReader(t *testing.T) {
	n := fixtureNative(t)
	ctx := context.Background()
	db, _, err := n.state(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := n.state(ctx, false)
	if err == nil {
		reader.Close()
		t.Fatal("startup reader observed old state while exclusive connection was held")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	reader, _, err = n.state(ctx, false)
	if err == nil {
		reader.Close()
		t.Fatal("exclusive connection released its lock before close")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reader, _, err = n.state(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
}

func TestNativeWriterBusy(t *testing.T) {
	n := fixtureNative(t)
	g, err := acquireNativeWriter(n.cfg.Home, fixtureThreadID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !nativeWriterBusy(n.cfg.Home, fixtureThreadID) {
		t.Fatal("live writer was not detected")
	}
	if err := syscall.Flock(int(g.thread.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if nativeWriterBusy(n.cfg.Home, fixtureThreadID) {
		t.Fatal("unlocked writer was treated as busy")
	}
}

func TestNativeWriterGuardOnlyCleansNewLock(t *testing.T) {
	n := fixtureNative(t)
	lock := filepath.Join(n.cfg.Home, "thread-writer-locks", fixtureThreadID+".lock")
	g, err := acquireNativeWriter(n.cfg.Home, fixtureThreadID)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new relay lock was left behind")
	}
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	g, err = acquireNativeWriter(n.cfg.Home, fixtureThreadID)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("preexisting native lock was removed")
	}
}

func TestNativeImmutablePermissionsCompatibility(t *testing.T) {
	for _, mode := range []string{"new", "existing"} {
		t.Run(mode, func(t *testing.T) {
			n := fixtureNative(t)
			ctx := context.Background()
			if mode == "existing" {
				if r, err := n.Install(ctx, fixtureBundle(t, "paginated", 0)); err != nil || r.Status != "installed" {
					t.Fatalf("%+v %v", r, err)
				}
			}
			b := fixtureBundle(t, "paginated", time.Second)
			// A native reader reapplies these immutable turn-context permissions to
			// SQLite, so an initial safe row override cannot protect the destination.
			line, err := json.Marshal(map[string]any{"type": "turn_context", "timestamp": time.Unix(0, b.Entry.LastEventNS).UTC().Format(time.RFC3339Nano), "ordinal": int64(8), "payload": map[string]any{"turn_id": "turn-1", "sandbox_policy": map[string]any{"type": "danger-full-access"}, "approval_policy": "never"}})
			if err != nil {
				t.Fatal(err)
			}
			b.Raw = append(b.Raw, append(line, '\n')...)
			info, err := inspectNativeRaw(b.Raw, b.Entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			b.Entry.Digest = info.digest
			b.History["thread_history_projection_state"][0]["next_rollout_byte_offset"] = int64(len(b.Raw))
			b.History["thread_history_projection_state"][0]["next_rollout_ordinal"] = int64(9)
			r, err := n.Install(ctx, b)
			if err != nil || r.Status != "deferred" || !strings.Contains(r.Reason, "permissions") {
				t.Fatalf("%+v %v", r, err)
			}
			if mode == "existing" {
				row := fixtureReadRow(t, n)
				if row["sandbox_policy"] != nativeReadOnlyProfile || row["approval_mode"] != "on-request" {
					t.Fatal("destination permissions were replaced")
				}
			}
		})
	}
	if !nativePermissionEqual(map[string]any{"type": "danger-full-access"}, `{"type":"disabled"}`) {
		t.Fatal("native equivalent disabled sandbox spelling was rejected")
	}
	if !nativePermissionEqual(map[string]any{"type": "read-only"}, nativeReadOnlyProfile) || nativePermissionEqual(map[string]any{"type": "read-only", "network_access": true}, nativeReadOnlyProfile) {
		t.Fatal("read-only compatibility normalization widened network permissions")
	}
}

func TestNativeRepeatedMetadataAndMetadataOnlyTimestamp(t *testing.T) {
	stamp := "2026-10-09T00:00:00.123456789Z"
	line, err := json.Marshal(map[string]any{"type": "session_meta", "timestamp": stamp, "payload": map[string]any{"id": fixtureThreadID, "history_mode": "legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	raw := append(append([]byte{}, line...), '\n')
	info, err := inspectNativeRaw(raw, fixtureThreadID)
	if err != nil || info.lastNS != time.Date(2026, 10, 9, 0, 0, 0, 123456789, time.UTC).UnixNano() {
		t.Fatalf("metadata-only timestamp: %+v %v", info, err)
	}
	info, err = inspectNativeRaw(append(raw, raw...), fixtureThreadID)
	if err != nil || info.dependencyReason != "" {
		t.Fatalf("same-identity repeated metadata was rejected: %+v %v", info, err)
	}
}

func TestNativeAuthoritativePermissionRecords(t *testing.T) {
	var readonly map[string]any
	if err := decodeJSON([]byte(nativeReadOnlyProfile), &readonly); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind string
		payload    map[string]any
		status     string
	}{
		{"modern_disabled_legacy_readonly", "turn_context", map[string]any{"sandbox_policy": map[string]any{"type": "read-only"}, "permission_profile": map[string]any{"type": "disabled"}, "approval_policy": "on-request"}, "deferred"},
		{"modern_readonly_legacy_disabled", "turn_context", map[string]any{"sandbox_policy": map[string]any{"type": "danger-full-access"}, "permission_profile": readonly, "approval_policy": "on-request"}, "installed"},
		{"profile_only_disabled", "turn_context", map[string]any{"permission_profile": map[string]any{"type": "disabled"}, "approval_policy": "on-request"}, "deferred"},
		{"profile_only_readonly", "turn_context", map[string]any{"permission_profile": readonly, "approval_policy": "on-request"}, "installed"},
		{"filesystem_fallback", "turn_context", map[string]any{"sandbox_policy": map[string]any{"type": "read-only"}, "file_system_sandbox_policy": map[string]any{"type": "unrestricted"}, "approval_policy": "on-request"}, "deferred"},
		{"settings_override", "event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"permission_profile": map[string]any{"type": "disabled"}, "approval_policy": "on-request"}}, "deferred"},
		{"settings_approval_override", "event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"permission_profile": readonly, "approval_policy": "never"}}, "deferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := fixtureNative(t)
			b := fixtureBundle(t, "paginated", 0)
			line, err := json.Marshal(map[string]any{"type": tc.kind, "timestamp": time.Unix(0, b.Entry.LastEventNS).UTC().Format(time.RFC3339Nano), "ordinal": int64(8), "payload": tc.payload})
			if err != nil {
				t.Fatal(err)
			}
			b.Raw = append(b.Raw, append(line, '\n')...)
			info, err := inspectNativeRaw(b.Raw, b.Entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			b.Entry.Digest = info.digest
			b.History["thread_history_projection_state"][0]["next_rollout_byte_offset"] = int64(len(b.Raw))
			b.History["thread_history_projection_state"][0]["next_rollout_ordinal"] = int64(9)
			r, err := n.Install(context.Background(), b)
			if err != nil || r.Status != tc.status {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestNativeBackfillPublicationGate(t *testing.T) {
	for _, status := range []string{"running", "missing"} {
		t.Run(status, func(t *testing.T) {
			n := fixtureNative(t)
			db, _, err := n.state(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			query := "UPDATE backfill_state SET status='running'"
			if status == "missing" {
				query = "DROP TABLE backfill_state"
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			db.Close()
			r, err := n.Install(context.Background(), fixtureBundle(t, "paginated", 0))
			if err != nil || r.Status != "deferred" || !strings.Contains(r.Reason, "backfill") {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err := os.Stat(filepath.Join(n.cfg.Home, "sessions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("incomplete native backfill allowed publication")
			}
		})
	}
}

func TestNativeCanonicalizesExistingLegacyPolicyStorage(t *testing.T) {
	n := fixtureNative(t)
	ctx := context.Background()
	if r, err := n.Install(ctx, fixtureBundle(t, "paginated", 0)); err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	db, _, err := n.state(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE threads SET sandbox_policy=? WHERE id=?", `{"type":"read-only"}`, fixtureThreadID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if r, err := n.Install(ctx, fixtureBundle(t, "paginated", time.Second)); err != nil || r.Status != "installed" {
		t.Fatalf("%+v %v", r, err)
	}
	if row := fixtureReadRow(t, n); row["sandbox_policy"] != nativeReadOnlyProfile {
		t.Fatal("legacy policy was left in invalid native storage format")
	}
	if policy, err := nativeCanonicalSandbox(`{"type":"danger-full-access"}`); err != nil || policy != `{"type":"disabled"}` {
		t.Fatalf("%q %v", policy, err)
	}
	if _, err := nativeCanonicalSandbox(`{"type":"workspace-write"}`); err == nil {
		t.Fatal("unsupported legacy workspace policy was accepted")
	}
}
