package historysync

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Keep this adapter limited to the reviewed native 0.161.0 / 0.162.0-alpha.2
// layout. In particular, never replace a native database or change rollout bytes.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const nativeMaxRawBytes = 512 << 20

// rust-v0.161.0 protocol PermissionProfile::read_only() serializes the native
// row as Managed, rather than the legacy rollout SandboxPolicy spelling.
const nativeReadOnlyProfile = `{"type":"managed","file_system":{"type":"restricted","entries":[{"path":{"type":"special","value":{"kind":"root"}},"access":"read"}]},"network":"restricted"}`

var nativeHistoryColumns = map[string][]string{
	"thread_history_projection_state": {"thread_id", "next_rollout_byte_offset", "next_rollout_ordinal"},
	"thread_items":                    {"thread_id", "turn_id", "item_id", "rollout_ordinal", "created_at_ms", "item_json", "item_type", "updated_at_ordinal", "started_at_ms", "completed_at_ms"},
	"thread_turns":                    {"thread_id", "turn_id", "rollout_ordinal", "status", "error_json", "started_at", "completed_at", "duration_ms", "first_user_item_id", "final_agent_item_id", "rollout_byte_offset", "rollout_end_ordinal", "rollout_end_byte_offset"},
	"thread_realtime_items":           {"thread_id", "item_id", "rollout_ordinal", "created_at_ms", "item_type", "item_json"},
}

var nativeThreadColumns = strings.Fields("id rollout_path created_at updated_at source model_provider cwd title sandbox_policy approval_mode tokens_used has_user_event archived archived_at git_sha git_branch git_origin_url cli_version first_user_message agent_nickname agent_role memory_mode model reasoning_effort agent_path created_at_ms updated_at_ms thread_source preview recency_at recency_at_ms history_mode name is_pinned thread_section_id section_position section_entered_at_ms project_id originator daybreak_enabled creator_user_id creator_account_id")
var nativeToolColumns = strings.Fields("thread_id position name description input_schema defer_loading namespace")

// A history winner never changes the destination's personal organization,
// security settings, credentials, or account identity.
var nativePreservedColumns = map[string]bool{
	"archived": true, "archived_at": true, "is_pinned": true,
	"thread_section_id": true, "section_position": true, "section_entered_at_ms": true,
	"project_id": true, "creator_user_id": true, "creator_account_id": true,
	"sandbox_policy": true, "approval_mode": true, "memory_mode": true,
	"daybreak_enabled": true, "originator": true, "name": true,
	"recency_at": true, "recency_at_ms": true,
}

type nativeCachedEntry struct {
	Path          string            `json:"path"`
	Size          int64             `json:"size"`
	ModifiedNS    int64             `json:"modified_ns"`
	Inode         uint64            `json:"inode"`
	ChangedNS     int64             `json:"changed_ns"`
	NextOrdinal   int64             `json:"next_ordinal,omitempty"`
	ExpectedTurns map[string]string `json:"expected_turns,omitempty"`
	ExpectsItems  bool              `json:"expects_items,omitempty"`
	Entry         Entry             `json:"entry"`
}

type nativeAdapter struct {
	cfg   Config
	mu    sync.Mutex
	cache map[string]nativeCachedEntry
	// Test seams never alter the production guard. It fails closed on ps errors.
	processGuard    func(context.Context) (bool, error)
	publicationHook func(string) error
}

func NewNative(cfg Config) (Native, error) {
	if !filepath.IsAbs(cfg.Home) || !filepath.IsAbs(cfg.Store) {
		return nil, errors.New("native home and sync store must be absolute")
	}
	resolvedHome, err := nativeResolvedPath(cfg.Home)
	if err != nil {
		return nil, err
	}
	resolvedStore, err := nativeResolvedPath(cfg.Store)
	if err != nil {
		return nil, err
	}
	if insidePath(resolvedHome, resolvedStore) {
		return nil, errors.New("sync store must be outside native Codex home")
	}
	n := &nativeAdapter{cfg: cfg, cache: make(map[string]nativeCachedEntry), processGuard: nativeProcessesRunning}
	if raw, err := os.ReadFile(filepath.Join(cfg.Store, "native-inventory-cache.json")); err == nil {
		_ = decodeJSON(raw, &n.cache)
		if n.cache == nil {
			n.cache = make(map[string]nativeCachedEntry)
		}
	}
	return n, nil
}

func nativeResolvedPath(filename string) (string, error) {
	current := filepath.Clean(filename)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// Stat field spellings differ between Darwin and Linux. Missing identity fields
// disable reuse, rather than trusting only modification time and byte count.
func nativeFileIdentity(info os.FileInfo) (uint64, int64) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, 0
	}
	ino := v.FieldByName("Ino")
	if !ino.IsValid() || !ino.CanUint() {
		return 0, 0
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		ct := v.FieldByName(name)
		if ct.IsValid() && ct.Kind() == reflect.Struct {
			sec, ns := ct.FieldByName("Sec"), ct.FieldByName("Nsec")
			if sec.IsValid() && ns.IsValid() && sec.CanInt() && ns.CanInt() {
				return ino.Uint(), sec.Int()*int64(time.Second) + ns.Int()
			}
		}
	}
	return ino.Uint(), 0
}

func insidePath(root, child string) bool {
	r, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func openNativeDB(filename string, writable bool) (*sql.DB, error) {
	if _, err := os.Stat(filename); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filename}
	q := u.Query()
	if !writable {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rw")
		q.Set("_txlock", "exclusive")
	}
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

type nativeQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func nativeRows(ctx context.Context, db nativeQuerier, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			if b, ok := values[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = values[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func nativeSchema(ctx context.Context, db nativeQuerier, table string, allowed []string, exact bool) ([]string, error) {
	rows, err := nativeRows(ctx, db, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("unsupported native schema: missing %s", table)
	}
	known := make(map[string]bool, len(allowed))
	for _, col := range allowed {
		known[col] = true
	}
	var cols []string
	for _, row := range rows {
		col, _ := row["name"].(string)
		if !known[col] {
			return nil, fmt.Errorf("unsupported native column %s.%s", table, col)
		}
		cols = append(cols, col)
	}
	if exact && len(cols) != len(allowed) {
		return nil, fmt.Errorf("unsupported native schema for %s", table)
	}
	return cols, nil
}

func (n *nativeAdapter) state(ctx context.Context, writable bool) (*sql.DB, []string, error) {
	db, err := openNativeDB(filepath.Join(n.cfg.Home, "state_5.sqlite"), writable)
	if err != nil {
		return nil, nil, err
	}
	cols, err := nativeSchema(ctx, db, "threads", nativeThreadColumns, false)
	if err == nil {
		for _, required := range []string{"id", "rollout_path", "history_mode", "archived", "cwd", "updated_at"} {
			if !containsNativeColumn(cols, required) {
				err = fmt.Errorf("unsupported native schema: threads.%s absent", required)
				break
			}
		}
	}
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, cols, nil
}

func containsNativeColumn(cols []string, col string) bool {
	for _, c := range cols {
		if c == col {
			return true
		}
	}
	return false
}

func nativeNumber(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case json.Number:
		return v.Int64()
	case nil:
		return 0, errors.New("missing integer")
	default:
		return 0, fmt.Errorf("unexpected integer type %T", value)
	}
}

func nativeArchived(row map[string]any) bool { x, _ := nativeNumber(row["archived"]); return x != 0 }

func nativeRolloutID(filename, stableID string) (string, error) {
	base := strings.TrimSuffix(filepath.Base(filename), ".jsonl")
	if strings.HasSuffix(filename, ".zst") {
		return "", errors.New("compressed rollouts require an explicit native decoder")
	}
	if !strings.HasPrefix(base, "rollout-") || len(base) < 36 {
		return "", errors.New("noncanonical rollout filename")
	}
	id := base[len(base)-36:]
	if !uuidPattern.MatchString(id) {
		return "", errors.New("noncanonical rollout ID")
	}
	if id != stableID && !strings.HasSuffix(base, stableID+"_"+id) {
		return "", errors.New("rollout filename belongs to another thread")
	}
	return id, nil
}

func (n *nativeAdapter) managedRollout(filename string) error {
	root, err := filepath.EvalSymlinks(n.cfg.Home)
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return err
	}
	if !insidePath(filepath.Join(root, "sessions"), actual) && !insidePath(filepath.Join(root, "archived_sessions"), actual) {
		return errors.New("rollout is outside managed native roots")
	}
	return nil
}

type nativeRawInfo struct {
	digest               string
	lastNS               int64
	nextOrdinal          int64
	mode                 string
	boundaries           map[int64]bool
	starts               map[int64]int64
	ends                 map[int64]int64
	dependencyReason     string
	expectedTurns        map[string]string
	expectsItems         bool
	sandboxPolicy        any
	permissionProfile    any
	approvalPolicy       any
	permissionUnresolved bool
}

func inspectNativeRaw(raw []byte, stableID string) (nativeRawInfo, error) {
	info := nativeRawInfo{boundaries: map[int64]bool{0: true}, starts: make(map[int64]int64), ends: make(map[int64]int64), expectedTurns: make(map[string]string)}
	if len(raw) == 0 || len(raw) > nativeMaxRawBytes || raw[len(raw)-1] != '\n' {
		return info, errors.New("raw history is empty, oversized, or not fully persisted")
	}
	seenMeta := false
	metadataNS := int64(0)
	expectedOrdinal := int64(0)
	offset := int64(0)
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		start := offset
		offset += int64(len(line))
		info.boundaries[offset] = true
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record map[string]any
		if !json.Valid(line) {
			return info, errors.New("malformed persisted JSONL")
		}
		if err := decodeJSON(line, &record); err != nil || record == nil {
			return info, errors.New("malformed persisted JSONL")
		}
		kind, _ := record["type"].(string)
		if seenMeta && kind == "session_meta" {
			payload, _ := record["payload"].(map[string]any)
			if payload["id"] != stableID {
				info.dependencyReason = "repeated session metadata has another thread identity"
			}
			if mode, ok := payload["history_mode"].(string); ok && mode != "" && mode != info.mode {
				info.dependencyReason = "repeated session metadata changes history mode"
			}
			stamp, _ := record["timestamp"].(string)
			created, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil || created.UnixNano() < 0 {
				return info, errors.New("session metadata has no valid persisted timestamp")
			}
			if created.UnixNano() > metadataNS {
				metadataNS = created.UnixNano()
			}
		}
		if !seenMeta {
			payload, _ := record["payload"].(map[string]any)
			if kind != "session_meta" || payload["id"] != stableID {
				return info, errors.New("raw session identity does not match stable thread ID")
			}
			seenMeta = true
			stamp, _ := record["timestamp"].(string)
			created, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil || created.UnixNano() < 0 {
				return info, errors.New("session metadata has no valid persisted timestamp")
			}
			metadataNS = created.UnixNano()
			info.mode, _ = payload["history_mode"].(string)
			if info.mode == "" {
				info.mode = "legacy"
			}
			if info.mode != "legacy" && info.mode != "paginated" {
				return info, errors.New("unsupported history mode")
			}
			if payload["history_base"] != nil {
				info.dependencyReason = "history_base lineage is not included in this raw bundle"
			}
			if start := payload["subagent_history_start_ordinal"]; start != nil {
				ordinal, err := nativeNumber(start)
				if err != nil || ordinal < 0 {
					return info, errors.New("invalid subagent history boundary")
				}
			}
		}
		if nativeExternalDependency(record) {
			info.dependencyReason = "external attachment or rollout dependency is not included in this bundle"
		}
		if info.mode == "paginated" {
			ordinal, err := nativeNumber(record["ordinal"])
			if err != nil || ordinal < 0 || ordinal == int64(^uint64(0)>>1) {
				return info, errors.New("paginated history has an invalid ordinal")
			}
			// Native materialization supports forward gaps; the next ordinal is
			// the last durable ordinal plus one, not the number of physical lines.
			if ordinal < expectedOrdinal {
				info.dependencyReason = "regressed paginated ordinals require native repair"
			}
			info.starts[ordinal], info.ends[ordinal] = start, offset
			if ordinal >= expectedOrdinal {
				expectedOrdinal = ordinal + 1
			}
		}
		if kind != "session_meta" {
			switch kind {
			case "event_msg", "response_item", "turn_context", "compacted", "realtime_item", "world_state", "token_usage_record", "inter_agent_communication_metadata":
			default:
				info.dependencyReason = fmt.Sprintf("unsupported durable record type %q", kind)
			}
			timestamp, _ := record["timestamp"].(string)
			t, err := time.Parse(time.RFC3339Nano, timestamp)
			if err != nil || t.UnixNano() < 0 {
				return info, errors.New("persisted history record has no valid timestamp")
			}
			if t.UnixNano() > info.lastNS {
				info.lastNS = t.UnixNano()
			}
			payload, _ := record["payload"].(map[string]any)
			if kind == "turn_context" {
				info.sandboxPolicy = payload["sandbox_policy"]
				info.permissionProfile = payload["permission_profile"]
				info.approvalPolicy = payload["approval_policy"]
				info.permissionUnresolved = info.permissionProfile == nil && payload["file_system_sandbox_policy"] != nil
			}
			payloadType, _ := payload["type"].(string)
			if kind == "event_msg" && payloadType == "thread_settings_applied" {
				settings, ok := payload["thread_settings"].(map[string]any)
				if !ok || settings["permission_profile"] == nil {
					info.permissionUnresolved = true
				} else {
					info.permissionProfile = settings["permission_profile"]
					info.permissionUnresolved = false
				}
				if value := settings["approval_policy"]; value != nil {
					info.approvalPolicy = value
				}
			}
			turnID, _ := payload["turn_id"].(string)
			if kind == "event_msg" && turnID != "" {
				switch payloadType {
				case "task_started":
					info.expectedTurns[turnID] = "inProgress"
				case "task_complete":
					info.expectedTurns[turnID] = "completed"
					if payload["error"] != nil {
						info.expectedTurns[turnID] = "failed"
					}
				case "turn_aborted":
					info.expectedTurns[turnID] = "interrupted"
				}
			}
			if kind == "event_msg" && (payloadType == "user_message" || payloadType == "agent_message" || payloadType == "item_completed") {
				info.expectsItems = true
			}
			if kind == "response_item" && (payloadType == "agent_message" || (payloadType == "message" && (payload["role"] == "user" || payload["role"] == "assistant"))) {
				info.expectsItems = true
			}
		}
	}
	if !seenMeta {
		return info, errors.New("no session metadata")
	}
	if info.lastNS == 0 {
		info.lastNS = metadataNS
	}
	info.nextOrdinal = expectedOrdinal
	if info.permissionUnresolved {
		info.dependencyReason = "raw filesystem permission fallback cannot be safely normalized"
	}
	h := sha256.Sum256(raw)
	info.digest = hex.EncodeToString(h[:])
	return info, nil
}

func nativeExternalDependency(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "history_base" && child != nil {
				return true
			}
			if k == "local_images" || k == "attachments" || k == "rollout_link" || k == "rollout_reference" {
				if a, ok := child.([]any); ok && len(a) > 0 {
					return true
				}
				if _, ok := child.(map[string]any); ok {
					return true
				}
			}
			if k == "image_url" || k == "file_url" || k == "audio_url" {
				if s, ok := child.(string); ok && !strings.HasPrefix(s, "data:") && (strings.HasPrefix(s, "file:") || filepath.IsAbs(s)) {
					return true
				}
			}
			if nativeExternalDependency(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if nativeExternalDependency(child) {
				return true
			}
		}
	}
	return false
}

func readNativeRaw(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > nativeMaxRawBytes {
		return nil, errors.New("rollout is not a supported regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, nativeMaxRawBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	beforeInode, beforeChanged := nativeFileIdentity(before)
	afterInode, afterChanged := nativeFileIdentity(after)
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || beforeInode != afterInode || beforeChanged != afterChanged || int64(len(raw)) != after.Size() {
		return nil, errors.New("rollout changed during read")
	}
	return raw, nil
}

func (n *nativeAdapter) Inventory(ctx context.Context) (map[string]Entry, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	db, _, err := n.state(ctx, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := nativeRows(ctx, db, "SELECT id, rollout_path, archived, history_mode FROM threads")
	if err != nil {
		return nil, err
	}
	entries := make(map[string]Entry, len(rows))
	history, historyErr := openNativeDB(filepath.Join(n.cfg.Home, "thread_history_1.sqlite"), false)
	if history != nil {
		defer history.Close()
	}
	for _, row := range rows {
		id, _ := row["id"].(string)
		filename, _ := row["rollout_path"].(string)
		entry := Entry{ID: id, Archived: nativeArchived(row)}
		if !uuidPattern.MatchString(id) {
			entry.Error = "unsupported thread ID"
			entries[id] = entry
			continue
		}
		if err := n.managedRollout(filename); err != nil {
			entry.Error = err.Error()
			entries[id] = entry
			continue
		}
		rolloutID, err := nativeRolloutID(filename, id)
		if err != nil {
			entry.Error = err.Error()
			entries[id] = entry
			continue
		}
		stat, err := os.Stat(filename)
		if err != nil {
			entry.Error = err.Error()
			entries[id] = entry
			continue
		}
		cached, ok := n.cache[id]
		inode, changed := nativeFileIdentity(stat)
		var info nativeRawInfo
		if ok && inode != 0 && changed != 0 && (row["history_mode"] != "paginated" || cached.NextOrdinal > 0) && cached.Path == filename && cached.Size == stat.Size() && cached.ModifiedNS == stat.ModTime().UnixNano() && cached.Inode == inode && cached.ChangedNS == changed {
			entry = cached.Entry
			entry.Archived = nativeArchived(row)
			info = nativeRawInfo{nextOrdinal: cached.NextOrdinal, expectedTurns: cached.ExpectedTurns, expectsItems: cached.ExpectsItems}
		} else {
			raw, readErr := readNativeRaw(filename)
			if readErr == nil {
				info, readErr = inspectNativeRaw(raw, id)
				if readErr == nil {
					entry.Digest = info.digest
					entry.LastEventNS = info.lastNS
					entry.RolloutID = rolloutID
					entry.DependencyReason = info.dependencyReason
				}
			}
			if readErr != nil {
				entry.Error = readErr.Error()
			} else {
				n.cache[id] = nativeCachedEntry{Path: filename, Size: stat.Size(), ModifiedNS: stat.ModTime().UnixNano(), Inode: inode, ChangedNS: changed, NextOrdinal: info.nextOrdinal, ExpectedTurns: info.expectedTurns, ExpectsItems: info.expectsItems, Entry: entry}
			}
		}
		if entry.Error == "" {
			if dep := nativeAttachmentReason(ctx, db, id); dep != "" {
				entry.DependencyReason = dep
			}
			if row["history_mode"] == "paginated" && entry.DependencyReason == "" {
				if historyErr != nil {
					entry.DependencyReason = "native projection unavailable: " + historyErr.Error()
				} else if err := nativeProjectionSummary(ctx, history, rolloutID, stat.Size(), info); err != nil {
					entry.DependencyReason = err.Error()
				}
			}
		}
		entry.Busy = nativeWriterBusy(n.cfg.Home, id)
		entries[id] = entry
	}
	// Inventory is read-only; the in-memory cache is persisted by Export only.
	return entries, nil
}

func nativeAttachmentReason(ctx context.Context, db nativeQuerier, id string) string {
	rows, err := nativeRows(ctx, db, "SELECT COUNT(*) AS count FROM thread_attachments WHERE thread_id = ?", id)
	if err != nil {
		return "attachment safety check failed: " + err.Error()
	}
	count, err := nativeNumber(rows[0]["count"])
	if err != nil || count != 0 {
		return "thread attachment dependency is not included in this bundle"
	}
	return ""
}

func nativeProjectionSummary(ctx context.Context, db nativeQuerier, id string, size int64, info nativeRawInfo) error {
	states, err := nativeRows(ctx, db, "SELECT next_rollout_byte_offset,next_rollout_ordinal FROM thread_history_projection_state WHERE thread_id = ?", id)
	if err != nil {
		return fmt.Errorf("native projection unavailable: %w", err)
	}
	if len(states) != 1 {
		return errors.New("paginated projection checkpoint missing")
	}
	offset, e1 := nativeNumber(states[0]["next_rollout_byte_offset"])
	ordinal, e2 := nativeNumber(states[0]["next_rollout_ordinal"])
	if e1 != nil || e2 != nil || offset != size || ordinal != info.nextOrdinal {
		return errors.New("paginated projection is behind or inconsistent with durable raw history")
	}
	turns, err := nativeRows(ctx, db, "SELECT turn_id,status FROM thread_turns WHERE thread_id = ?", id)
	if err != nil {
		return err
	}
	items, err := nativeRows(ctx, db, "SELECT COUNT(*) AS count FROM thread_items WHERE thread_id = ?", id)
	if err != nil {
		return err
	}
	count, err := nativeNumber(items[0]["count"])
	if err != nil {
		return err
	}
	return nativeMeaningfulProjection(turns, count, info)
}

func nativeMeaningfulProjection(turns []map[string]any, itemCount int64, info nativeRawInfo) error {
	seen := make(map[string]string, len(turns))
	for _, row := range turns {
		id, _ := row["turn_id"].(string)
		status, _ := row["status"].(string)
		seen[id] = status
	}
	for id, status := range info.expectedTurns {
		if actual, ok := seen[id]; !ok || actual != status {
			return errors.New("paginated projection is missing a durable turn or its latest status")
		}
	}
	if info.expectsItems && itemCount == 0 {
		return errors.New("paginated projection has no items for durable message history")
	}
	return nil
}

func nativeWriterBusy(home, id string) bool {
	f, err := os.Open(filepath.Join(home, "thread-writer-locks", id+".lock"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

type nativeWriterGuard struct {
	coordination, thread *os.File
	createdThread        bool
}

func acquireNativeWriter(home, id string) (*nativeWriterGuard, error) {
	if !uuidPattern.MatchString(id) {
		return nil, errors.New("invalid thread ID")
	}
	directory := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	coord, err := os.OpenFile(filepath.Join(directory, ".coordination.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(coord.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		coord.Close()
		return nil, fmt.Errorf("native coordination busy: %w", err)
	}
	threadPath := filepath.Join(directory, id+".lock")
	_, statErr := os.Lstat(threadPath)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		coord.Close()
		return nil, statErr
	}
	createdThread := errors.Is(statErr, os.ErrNotExist)
	thread, err := os.OpenFile(threadPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		coord.Close()
		return nil, err
	}
	if err := syscall.Flock(int(thread.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		thread.Close()
		coord.Close()
		return nil, fmt.Errorf("native thread writer busy: %w", err)
	}
	return &nativeWriterGuard{coordination: coord, thread: thread, createdThread: createdThread}, nil
}

func (g *nativeWriterGuard) Close() {
	g.thread.Close()
	// The native coordination lock prevents a new writer from opening the path
	// during removal. Never remove a lock that predated this relay operation.
	if g.createdThread {
		_ = os.Remove(g.thread.Name())
	}
	g.coordination.Close()
}

func (n *nativeAdapter) Export(ctx context.Context, id string) (*Bundle, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	guard, err := acquireNativeWriter(n.cfg.Home, id)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	db, _, err := n.state(ctx, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := nativeRows(ctx, tx, "SELECT * FROM threads WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("native thread not found")
	}
	row := rows[0]
	filename, _ := row["rollout_path"].(string)
	if err := n.managedRollout(filename); err != nil {
		return nil, err
	}
	rolloutID, err := nativeRolloutID(filename, id)
	if err != nil {
		return nil, err
	}
	raw, err := readNativeRaw(filename)
	if err != nil {
		return nil, err
	}
	info, err := inspectNativeRaw(raw, id)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Format: Format, Node: n.cfg.Node, SourceHome: n.cfg.Home, Entry: Entry{ID: id, Digest: info.digest, LastEventNS: info.lastNS, Archived: nativeArchived(row), RolloutID: rolloutID, DependencyReason: info.dependencyReason}, Raw: raw, Row: row, History: make(map[string][]map[string]any)}
	if row["history_mode"] != info.mode {
		b.Entry.DependencyReason = "native history mode disagrees with raw history"
	}
	if dep := nativeAttachmentReason(ctx, tx, id); dep != "" {
		b.Entry.DependencyReason = dep
	}
	if info.mode == "paginated" {
		if err := n.loadProjection(ctx, b); err != nil {
			b.Entry.DependencyReason = "native projection unavailable: " + err.Error()
		} else if err := validateNativeHistory(b, info); err != nil {
			b.Entry.DependencyReason = err.Error()
		}
	}
	if _, err := nativeSchema(ctx, tx, "thread_dynamic_tools", nativeToolColumns, false); err != nil {
		b.Entry.DependencyReason = err.Error()
	}
	b.Tools, err = nativeRows(ctx, tx, "SELECT * FROM thread_dynamic_tools WHERE thread_id = ? ORDER BY position", id)
	if err != nil {
		b.Entry.DependencyReason = err.Error()
	}
	if err := validateNativeTools(b.Tools, id); err != nil {
		b.Entry.DependencyReason = err.Error()
	}
	if stat, err := os.Stat(filename); err == nil {
		inode, changed := nativeFileIdentity(stat)
		cachedEntry := b.Entry
		cachedEntry.DependencyReason = info.dependencyReason // projection and attachments can change without changing raw bytes
		n.cache[id] = nativeCachedEntry{Path: filename, Size: stat.Size(), ModifiedNS: stat.ModTime().UnixNano(), Inode: inode, ChangedNS: changed, NextOrdinal: info.nextOrdinal, ExpectedTurns: info.expectedTurns, ExpectsItems: info.expectsItems, Entry: cachedEntry}
		if cache, err := json.Marshal(n.cache); err == nil {
			_ = nativePrivateWrite(filepath.Join(n.cfg.Store, "native-inventory-cache.json"), cache)
		}
	}
	return b, nil
}

func (n *nativeAdapter) loadProjection(ctx context.Context, b *Bundle) error {
	history, err := openNativeDB(filepath.Join(n.cfg.Home, "thread_history_1.sqlite"), false)
	if err != nil {
		return err
	}
	defer history.Close()
	tx, err := history.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for table, cols := range nativeHistoryColumns {
		if _, err := nativeSchema(ctx, tx, table, cols, true); err != nil {
			return err
		}
		b.History[table], err = nativeRows(ctx, tx, "SELECT * FROM "+table+" WHERE thread_id = ?", b.Entry.RolloutID)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateNativeHistory(b *Bundle, info nativeRawInfo) error {
	for table, rows := range b.History {
		cols, ok := nativeHistoryColumns[table]
		if !ok {
			return fmt.Errorf("unsupported projection table %s", table)
		}
		for _, row := range rows {
			for col := range row {
				if !containsNativeColumn(cols, col) {
					return fmt.Errorf("unsupported projection field %s.%s", table, col)
				}
			}
			if row["thread_id"] != b.Entry.RolloutID {
				return errors.New("projection belongs to another rollout")
			}
			for _, field := range []string{"rollout_ordinal", "updated_at_ordinal", "rollout_end_ordinal"} {
				if row[field] == nil {
					continue
				}
				x, err := nativeNumber(row[field])
				if err != nil || x < 0 || x >= info.nextOrdinal {
					return errors.New("projection ordinal exceeds durable history")
				}
			}
			for _, field := range []string{"rollout_byte_offset", "rollout_end_byte_offset"} {
				if row[field] == nil {
					continue
				}
				x, err := nativeNumber(row[field])
				if err != nil || !info.boundaries[x] {
					return errors.New("projection offset is not a durable line boundary")
				}
			}
		}
	}
	if info.mode == "legacy" {
		for _, rows := range b.History {
			if len(rows) != 0 {
				return errors.New("legacy bundle unexpectedly contains paginated projection")
			}
		}
		return nil
	}
	states := b.History["thread_history_projection_state"]
	if len(states) != 1 {
		return errors.New("paginated projection checkpoint missing")
	}
	offset, e1 := nativeNumber(states[0]["next_rollout_byte_offset"])
	ordinal, e2 := nativeNumber(states[0]["next_rollout_ordinal"])
	if e1 != nil || e2 != nil || offset != int64(len(b.Raw)) || ordinal != info.nextOrdinal {
		return errors.New("paginated projection is behind or inconsistent with durable raw history")
	}
	for table := range nativeHistoryColumns {
		if _, ok := b.History[table]; !ok {
			return fmt.Errorf("projection table %s missing", table)
		}
	}
	if err := nativeMeaningfulProjection(b.History["thread_turns"], int64(len(b.History["thread_items"])), info); err != nil {
		return err
	}
	items := make(map[string]string)
	for _, row := range b.History["thread_items"] {
		id, _ := row["item_id"].(string)
		turn, _ := row["turn_id"].(string)
		items[id] = turn
	}
	for _, row := range b.History["thread_turns"] {
		turn, _ := row["turn_id"].(string)
		for _, col := range []string{"first_user_item_id", "final_agent_item_id"} {
			if id, ok := row[col].(string); ok && id != "" && items[id] != turn {
				return errors.New("projection turn references an absent item")
			}
		}
		for _, pair := range [][2]string{{"rollout_ordinal", "rollout_byte_offset"}, {"rollout_end_ordinal", "rollout_end_byte_offset"}} {
			if row[pair[1]] == nil {
				continue
			}
			ordinal, _ := nativeNumber(row[pair[0]])
			offset, _ := nativeNumber(row[pair[1]])
			expected, ok := info.starts[ordinal]
			if pair[1] == "rollout_end_byte_offset" {
				expected, ok = info.ends[ordinal]
			}
			if !ok || expected != offset {
				return errors.New("projection ordinal and byte offset refer to different raw records")
			}
		}
	}
	return nil
}

func validateNativeTools(rows []map[string]any, id string) error {
	for _, row := range rows {
		if row["thread_id"] != id {
			return errors.New("dynamic tool belongs to another thread")
		}
		for col := range row {
			if !containsNativeColumn(nativeToolColumns, col) {
				return fmt.Errorf("unsupported dynamic tool column %s", col)
			}
		}
	}
	return nil
}

func validateNativeBundle(b *Bundle) (nativeRawInfo, error) {
	if b == nil || b.Format != Format || !uuidPattern.MatchString(b.Entry.ID) || !uuidPattern.MatchString(b.Entry.RolloutID) {
		return nativeRawInfo{}, errors.New("invalid native bundle identity or format")
	}
	info, err := inspectNativeRaw(b.Raw, b.Entry.ID)
	if err != nil {
		return info, err
	}
	if info.digest != b.Entry.Digest || info.lastNS != b.Entry.LastEventNS {
		return info, errors.New("bundle digest or actual persisted timestamp disagrees with raw history")
	}
	if b.Row["id"] != b.Entry.ID || b.Row["history_mode"] != info.mode {
		return info, errors.New("bundle state row identity or history mode mismatch")
	}
	if nativeArchived(b.Row) != b.Entry.Archived {
		return info, errors.New("bundle archive metadata disagrees")
	}
	for col := range b.Row {
		if !containsNativeColumn(nativeThreadColumns, col) {
			return info, fmt.Errorf("unsupported state column %s", col)
		}
	}
	if b.Entry.DependencyReason == "" && info.dependencyReason == "" {
		if err := validateNativeHistory(b, info); err != nil {
			return info, err
		}
		if err := validateNativeTools(b.Tools, b.Entry.ID); err != nil {
			return info, err
		}
	}
	return info, nil
}

func nativePrivateWrite(filename string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".history-sync-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, filename); err != nil {
		return err
	}
	return nativeSyncDir(filepath.Dir(filename))
}

func nativeSyncDir(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func nativeProcessesRunning(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,uid=,stat=,comm=,args=").Output()
	if err != nil {
		return false, fmt.Errorf("native process guard unavailable: %w", err)
	}
	return nativeScopedProcessListingRunning(out, os.Getpid(), os.Getuid())
}

func nativeProcessListingRunning(out []byte, selfPID int) (bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			if len(fields) == 0 {
				continue
			}
			return false, errors.New("native process listing is incomplete")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return false, errors.New("native process listing has an invalid PID")
		}
		if pid == selfPID {
			continue
		}
		command := strings.ToLower(fields[1])
		base := strings.ToLower(filepath.Base(command))
		argsBase := ""
		if len(fields) >= 3 {
			argsBase = strings.ToLower(filepath.Base(strings.Trim(fields[2], "\"'")))
		}
		if base == "codex-history-sync" || ((base == "codex-history-s" || base == "codex-history-sy") && argsBase == "codex-history-sync") {
			continue
		}
		lower := strings.ToLower(line)
		if base == "codex" || strings.HasPrefix(base, "codex-") || argsBase == "codex" || strings.HasPrefix(base, "chatgpt") || strings.Contains(lower, "/codex.app/") || strings.Contains(lower, "/chatgpt.app/") {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func nativeNewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func copyNativeRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

func nativeSQLValue(value any) (any, error) {
	if n, ok := value.(json.Number); ok {
		return n.Int64()
	}
	switch value.(type) {
	case nil, string, int64, int, bool:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported SQLite value %T", value)
	}
}

func nativeInsertRow(ctx context.Context, tx *sql.Tx, table string, row map[string]any, schema []string) error {
	var cols []string
	for col := range row {
		if !containsNativeColumn(schema, col) {
			return fmt.Errorf("unsupported target column %s.%s", table, col)
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	values := make([]any, len(cols))
	slots := make([]string, len(cols))
	for i, col := range cols {
		v, err := nativeSQLValue(row[col])
		if err != nil {
			return err
		}
		values[i] = v
		slots[i] = "?"
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(slots, ",")+")", values...)
	return err
}

func (n *nativeAdapter) Install(ctx context.Context, b *Bundle) (result InstallResult, resultErr error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	result = InstallResult{Status: "deferred"}
	if b != nil {
		result.ID = b.Entry.ID
		result.Digest = b.Entry.Digest
	}
	info, err := validateNativeBundle(b)
	if err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	if reason := info.dependencyReason; reason != "" {
		result.Reason = reason
		return result, nil
	}
	if b.Entry.DependencyReason != "" {
		result.Reason = b.Entry.DependencyReason
		return result, nil
	}
	// The relay retains its immutable pending object before calling Install.
	// Active runtimes therefore need no second durable copy every polling cycle.
	running, err := n.processGuard(ctx)
	if err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	if running {
		result.Reason = "native Codex processes are running; publication deferred"
		return result, nil
	}
	staged, err := json.Marshal(b)
	if err != nil {
		return result, err
	}
	stageHash := sha256.Sum256(staged)
	stageObjectDigest := hex.EncodeToString(stageHash[:])
	stagePath := filepath.Join(n.cfg.Store, "native-staging", b.Entry.ID, stageObjectDigest+".json")
	if err := nativePrivateWrite(stagePath, staged); err != nil {
		return result, err
	}
	guard, err := acquireNativeWriter(n.cfg.Home, b.Entry.ID)
	if err != nil {
		result.Status = "busy"
		result.Reason = err.Error()
		return result, nil
	}
	defer guard.Close()
	// Check twice: a runtime starting while the native lock was acquired must defer.
	running, err = n.processGuard(ctx)
	if err != nil || running {
		result.Reason = "native runtime started during publication preflight"
		return result, nil
	}
	state, columns, err := n.state(ctx, true)
	if err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	defer state.Close()
	// In WAL mode BEGIN EXCLUSIVE alone still allows readers. Connection-level
	// exclusive locking also blocks a runtime starting after our process check
	// from reading the old pointer and later backfilling it over the new one.
	if _, err := state.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	tx, err := state.BeginTx(ctx, nil)
	if err != nil {
		result.Reason = "native state could not be exclusively locked: " + err.Error()
		return result, nil
	}
	defer tx.Rollback()
	backfill, err := nativeRows(ctx, tx, "SELECT status FROM backfill_state WHERE id = 1")
	if err != nil || len(backfill) != 1 || backfill[0]["status"] != "complete" {
		result.Reason = "native backfill is missing or incomplete; publication deferred"
		return result, nil
	}
	running, err = n.processGuard(ctx)
	if err != nil || running {
		result.Reason = "native runtime started before exclusive publication lock"
		return result, nil
	}
	if _, err := nativeSchema(ctx, tx, "thread_dynamic_tools", nativeToolColumns, false); err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	var history *sql.DB
	if info.mode == "paginated" {
		history, err = openNativeDB(filepath.Join(n.cfg.Home, "thread_history_1.sqlite"), true)
		if err != nil {
			result.Reason = err.Error()
			return result, nil
		}
		defer history.Close()
		for table, cols := range nativeHistoryColumns {
			if _, err := nativeSchema(ctx, history, table, cols, true); err != nil {
				result.Reason = err.Error()
				return result, nil
			}
		}
	}
	existing, err := nativeRows(ctx, tx, "SELECT * FROM threads WHERE id = ?", b.Entry.ID)
	if err != nil {
		return result, err
	}
	var previous map[string]any
	var previousRaw []byte
	if len(existing) > 0 {
		previous = existing[0]
		if reason := nativeAttachmentReason(ctx, tx, b.Entry.ID); reason != "" {
			result.Reason = reason
			return result, nil
		}
		previousPath, _ := previous["rollout_path"].(string)
		if err := n.managedRollout(previousPath); err != nil {
			result.Reason = err.Error()
			return result, nil
		}
		previousRaw, err = readNativeRaw(previousPath)
		if err != nil {
			result.Reason = err.Error()
			return result, nil
		}
		old, err := inspectNativeRaw(previousRaw, b.Entry.ID)
		if err != nil {
			result.Reason = "destination history requires review: " + err.Error()
			return result, nil
		}
		if previous["history_mode"] != old.mode {
			result.Reason = "destination history mode disagrees with raw history"
			return result, nil
		}
		if old.dependencyReason != "" {
			result.Reason = "destination dependency requires review: " + old.dependencyReason
			return result, nil
		}
		result.PreviousDigest = old.digest
		if old.digest == info.digest {
			if previous["history_mode"] == "paginated" {
				localID, idErr := nativeRolloutID(previousPath, b.Entry.ID)
				if idErr != nil {
					result.Reason = idErr.Error()
					return result, nil
				}
				local := &Bundle{Entry: Entry{ID: b.Entry.ID, RolloutID: localID}, Raw: previousRaw, History: make(map[string][]map[string]any)}
				if err := n.loadProjection(ctx, local); err != nil {
					result.Reason = err.Error()
					return result, nil
				}
				if err := validateNativeHistory(local, old); err != nil {
					result.Reason = err.Error()
					return result, nil
				}
			}
			result.Status = "unchanged"
			return result, nil
		}
		if old.lastNS > info.lastNS {
			result.Status = "unchanged"
			result.Reason = "destination persisted history is newer"
			return result, nil
		}
		if old.lastNS == info.lastNS {
			result.Status = "conflict"
			result.Reason = "equal persisted timestamps with different raw histories"
			return result, nil
		}
		if previous["history_mode"] == "paginated" && info.mode == "legacy" {
			result.Reason = "native history mode cannot be downgraded"
			return result, nil
		}
	}
	localSandbox := nativeReadOnlyProfile
	localApproval := "on-request"
	policyPin := ""
	destinationPolicy := previous
	if previous != nil {
		localSandbox, err = nativeCanonicalSandbox(previous["sandbox_policy"])
		if err != nil {
			result.Reason = err.Error()
			return result, nil
		}
		localApproval, _ = previous["approval_mode"].(string)
	} else {
		localSandbox, localApproval, policyPin, err = n.newThreadPolicy()
		if err != nil {
			result.Reason = err.Error()
			return result, nil
		}
		destinationPolicy = map[string]any{"sandbox_policy": localSandbox, "approval_mode": localApproval}
	}
	if reason := nativePermissionCompatibility(info, destinationPolicy); reason != "" {
		result.Reason = reason
		return result, nil
	}
	newRolloutID, err := nativeNewUUID()
	if err != nil {
		return result, err
	}
	result.RolloutID = newRolloutID
	archived := b.Entry.Archived
	if previous != nil {
		archived = nativeArchived(previous)
	}
	now := time.Now().UTC()
	directory := filepath.Join(n.cfg.Home, "sessions", now.Format("2006/01/02"))
	if archived {
		directory = filepath.Join(n.cfg.Home, "archived_sessions")
	}
	filename := "rollout-" + now.Format("2006-01-02T15-04-05") + "-" + b.Entry.ID + "_" + newRolloutID + ".jsonl"
	newPath := filepath.Join(directory, filename)
	pointerCommitted := false
	defer func() {
		if !pointerCommitted {
			// Keep archived/staged raw outside managed roots. An unreferenced raw
			// file under sessions could otherwise be rediscovered at startup.
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("uncommitted rollout cleanup: %w", err))
			} else if err == nil {
				if err := nativeSyncDir(directory); err != nil {
					resultErr = errors.Join(resultErr, err)
				}
			}
		}
	}()
	journalPath := filepath.Join(n.cfg.Store, "native-journal", newRolloutID+".json")
	journal := map[string]any{"stable_id": b.Entry.ID, "rollout_id": newRolloutID, "digest": info.digest, "path": newPath, "phase": "prepared", "previous_row": previous, "stage": stagePath, "stage_object_digest": stageObjectDigest}
	if policyPin != "" {
		journal["new_thread_policy_config_sha256"] = policyPin
	}
	previousTools, err := nativeRows(ctx, tx, "SELECT * FROM thread_dynamic_tools WHERE thread_id = ?", b.Entry.ID)
	if err != nil {
		return result, err
	}
	journal["previous_tools"] = previousTools
	if previous != nil {
		backupPath := filepath.Join(n.cfg.Store, "native-backups", b.Entry.ID, result.PreviousDigest+".jsonl")
		if err := nativePrivateWrite(backupPath, previousRaw); err != nil {
			return result, err
		}
		journal["previous_raw_backup"] = backupPath
	}
	writeJournal := func(phase string) error {
		journal["phase"] = phase
		raw, err := json.Marshal(journal)
		if err != nil {
			return err
		}
		return nativePrivateWrite(journalPath, raw)
	}
	if err := writeJournal("prepared"); err != nil {
		return result, err
	}
	if n.publicationHook != nil {
		if err := n.publicationHook("prepared"); err != nil {
			return result, err
		}
	}
	// Raw publication and new projection keys are unreachable from the old state
	// pointer. A crash here leaves recoverable orphans, never a partly replaced chat.
	running, err = n.processGuard(ctx)
	if err != nil || running {
		result.Reason = "native runtime started before raw publication"
		return result, nil
	}
	if err := nativePrivateWrite(newPath, b.Raw); err != nil {
		return result, err
	}
	if err := writeJournal("raw_published"); err != nil {
		return result, err
	}
	if info.mode == "paginated" {
		running, err = n.processGuard(ctx)
		if err != nil || running {
			result.Reason = "native runtime started before projection publication"
			return result, nil
		}
		htx, err := history.BeginTx(ctx, nil)
		if err != nil {
			return result, err
		}
		defer htx.Rollback()
		for _, table := range []string{"thread_turns", "thread_items", "thread_realtime_items", "thread_history_projection_state"} {
			for _, sourceRow := range b.History[table] {
				row := copyNativeRow(sourceRow)
				row["thread_id"] = newRolloutID
				if err := nativeInsertRow(ctx, htx, table, row, nativeHistoryColumns[table]); err != nil {
					return result, err
				}
			}
		}
		if err := htx.Commit(); err != nil {
			return result, err
		}
	}
	if err := writeJournal("projection_ready"); err != nil {
		return result, err
	}
	if n.publicationHook != nil {
		if err := n.publicationHook("projection_ready"); err != nil {
			return result, err
		}
	}
	running, err = n.processGuard(ctx)
	if err != nil || running {
		result.Reason = "native runtime started; prepared generation remains pending"
		return result, nil
	}
	current, err := nativeRows(ctx, tx, "SELECT * FROM threads WHERE id = ?", b.Entry.ID)
	if err != nil {
		return result, err
	}
	if !nativeSameRow(current, previous) {
		result.Status = "conflict"
		result.Reason = "native thread changed during publication"
		return result, nil
	}
	row := copyNativeRow(b.Row)
	for key := range row {
		if !containsNativeColumn(columns, key) {
			delete(row, key)
		}
	}
	for key := range nativePreservedColumns {
		if !containsNativeColumn(columns, key) {
			continue
		}
		if previous != nil {
			if val, ok := previous[key]; ok {
				row[key] = val
			}
		} else {
			switch key {
			case "archived", "archived_at":
			case "sandbox_policy":
				row[key] = nativeReadOnlyProfile
			case "approval_mode":
				row[key] = "on-request"
			case "memory_mode":
				delete(row, key) // use the destination schema default
			case "is_pinned":
				row[key] = int64(0)
			default:
				row[key] = nil
			}
		}
	}
	row["sandbox_policy"] = localSandbox
	row["approval_mode"] = localApproval
	// Keep native provenance in raw history. Only SQLite's runtime cwd maps homes.
	if cwd, ok := row["cwd"].(string); ok {
		row["cwd"] = nativeMapCWD(cwd, b.SourceHome, n.cfg.Home)
	}
	row["rollout_path"] = newPath
	row["updated_at"] = b.Entry.LastEventNS / int64(time.Second)
	if containsNativeColumn(columns, "updated_at_ms") {
		row["updated_at_ms"] = b.Entry.LastEventNS / int64(time.Millisecond)
	}
	if previous == nil {
		if containsNativeColumn(columns, "recency_at") {
			row["recency_at"] = row["updated_at"]
		}
		if containsNativeColumn(columns, "recency_at_ms") {
			row["recency_at_ms"] = b.Entry.LastEventNS / int64(time.Millisecond)
		}
		if err := nativeInsertRow(ctx, tx, "threads", row, columns); err != nil {
			return result, err
		}
	} else {
		var keys []string
		for k := range row {
			if k != "id" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var assignments []string
		var values []any
		for _, key := range keys {
			value, err := nativeSQLValue(row[key])
			if err != nil {
				return result, err
			}
			assignments = append(assignments, key+" = ?")
			values = append(values, value)
		}
		values = append(values, b.Entry.ID, previous["rollout_path"])
		changed, err := tx.ExecContext(ctx, "UPDATE threads SET "+strings.Join(assignments, ",")+" WHERE id = ? AND rollout_path = ?", values...)
		if err != nil {
			return result, err
		}
		count, err := changed.RowsAffected()
		if err != nil || count != 1 {
			return result, errors.New("native rollout pointer CAS failed")
		}
	}
	toolColumns, err := nativeSchema(ctx, tx, "thread_dynamic_tools", nativeToolColumns, false)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM thread_dynamic_tools WHERE thread_id = ?", b.Entry.ID); err != nil {
		return result, err
	}
	for _, sourceRow := range b.Tools {
		tool := copyNativeRow(sourceRow)
		for col := range tool {
			if !containsNativeColumn(toolColumns, col) {
				delete(tool, col)
			}
		}
		if err := nativeInsertRow(ctx, tx, "thread_dynamic_tools", tool, toolColumns); err != nil {
			return result, err
		}
	}
	if policyPin != "" {
		if err := n.verifyPolicyConfigPin(policyPin); err != nil {
			result.Reason = err.Error()
			return result, nil
		}
	}
	running, err = n.processGuard(ctx)
	if err != nil || running {
		result.Reason = "native runtime started before native pointer commit"
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	pointerCommitted = true
	if err := writeJournal("committed"); err != nil {
		return result, fmt.Errorf("native pointer committed; journal completion failed: %w", err)
	}
	delete(n.cache, b.Entry.ID)
	result.Status = "installed"
	result.Reason = "immutable generation published with the same stable thread ID"
	return result, nil
}

func (n *nativeAdapter) newThreadPolicy() (string, string, string, error) {
	policy := n.cfg.NewThreadPolicy
	if policy == nil {
		return nativeReadOnlyProfile, "on-request", "", nil
	}
	if len(policy.ConfigSHA256) != sha256.Size*2 {
		return "", "", "", errors.New("new-thread destination policy config pin is invalid")
	}
	if _, err := hex.DecodeString(policy.ConfigSHA256); err != nil {
		return "", "", "", errors.New("new-thread destination policy config pin is invalid")
	}
	sandbox, err := nativeCanonicalSandbox(string(policy.Sandbox))
	if err != nil {
		return "", "", "", fmt.Errorf("new-thread destination policy: %w", err)
	}
	switch policy.Approval {
	case "never", "on-request", "on-failure", "untrusted":
	default:
		return "", "", "", errors.New("new-thread destination approval policy is unsupported")
	}
	pin := strings.ToLower(policy.ConfigSHA256)
	if err := n.verifyPolicyConfigPin(pin); err != nil {
		return "", "", "", err
	}
	return sandbox, policy.Approval, pin, nil
}

func (n *nativeAdapter) verifyPolicyConfigPin(pin string) error {
	filename := filepath.Join(n.cfg.Home, "config.toml")
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("new-thread destination config pin cannot be verified: %w", err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() > 32<<20 {
		return errors.New("new-thread destination config is not a supported regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 32<<20+1))
	if err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(filename)
	if err != nil {
		return err
	}
	bino, bctime := nativeFileIdentity(before)
	aino, actime := nativeFileIdentity(after)
	cino, cctime := nativeFileIdentity(current)
	if before.Size() != after.Size() || after.Size() != int64(len(raw)) || !before.ModTime().Equal(after.ModTime()) || bino != aino || bctime != actime || aino != cino || actime != cctime {
		return errors.New("new-thread destination config changed during policy verification")
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != pin {
		return errors.New("new-thread destination config changed; pinned policy requires review")
	}
	return nil
}

func nativePermissionCompatibility(info nativeRawInfo, previous map[string]any) string {
	sandbox, approval := any(nativeReadOnlyProfile), any("on-request")
	if previous != nil {
		sandbox, approval = previous["sandbox_policy"], previous["approval_mode"]
	}
	policy := info.permissionProfile
	if policy == nil {
		policy = info.sandboxPolicy
	}
	if policy == nil {
		policy = nativeReadOnlyProfile
	}
	if !nativePermissionEqual(policy, sandbox) {
		return "raw history sandbox permissions differ from destination policy; immutable raw publication deferred"
	}
	requestedApproval := info.approvalPolicy
	if requestedApproval == nil {
		requestedApproval = "on-request"
	}
	if !nativePermissionEqual(requestedApproval, approval) {
		return "raw history approval permissions differ from destination policy; immutable raw publication deferred"
	}
	return ""
}

func nativePermissionEqual(a, b any) bool {
	normalize := func(value any) any {
		if s, ok := value.(string); ok && json.Valid([]byte(s)) {
			var parsed any
			if decodeJSON([]byte(s), &parsed) == nil {
				value = parsed
			}
		}
		if m, ok := value.(map[string]any); ok && m["type"] == "disabled" {
			m = copyNativeRow(m)
			m["type"] = "danger-full-access"
			value = m
		}
		if m, ok := value.(map[string]any); ok && m["type"] == "read-only" && (len(m) == 1 || (len(m) == 2 && (m["network_access"] == false || m["network_access"] == true))) {
			var native any
			if decodeJSON([]byte(nativeReadOnlyProfile), &native) == nil {
				if m["network_access"] == true {
					native.(map[string]any)["network"] = "enabled"
				}
				value = native
			}
		}
		return value
	}
	ax, e1 := json.Marshal(normalize(a))
	bx, e2 := json.Marshal(normalize(b))
	return e1 == nil && e2 == nil && bytes.Equal(ax, bx)
}

func nativeCanonicalSandbox(value any) (string, error) {
	original, _ := value.(string)
	if original != "" {
		if !json.Valid([]byte(original)) {
			return "", errors.New("destination sandbox policy is not valid native JSON")
		}
		if err := decodeJSON([]byte(original), &value); err != nil {
			return "", errors.New("destination sandbox policy is not valid native JSON")
		}
	}
	m, ok := value.(map[string]any)
	if !ok {
		return "", errors.New("destination sandbox policy is unsupported")
	}
	switch m["type"] {
	case "danger-full-access":
		if len(m) != 1 {
			return "", errors.New("destination legacy sandbox policy requires native normalization")
		}
		return `{"type":"disabled"}`, nil
	case "read-only":
		if len(m) > 2 || (len(m) == 2 && m["network_access"] != false && m["network_access"] != true) {
			return "", errors.New("destination legacy sandbox policy requires native normalization")
		}
		if m["network_access"] != true {
			return nativeReadOnlyProfile, nil
		}
		var profile map[string]any
		_ = decodeJSON([]byte(nativeReadOnlyProfile), &profile)
		profile["network"] = "enabled"
		raw, _ := json.Marshal(profile)
		return string(raw), nil
	case "disabled":
		if len(m) != 1 {
			return "", errors.New("destination native sandbox policy is malformed")
		}
	case "managed":
		fs, ok := m["file_system"].(map[string]any)
		if !ok || (fs["type"] != "restricted" && fs["type"] != "unrestricted") || (m["network"] != "restricted" && m["network"] != "enabled") {
			return "", errors.New("destination native sandbox policy is malformed")
		}
		if fs["type"] == "restricted" {
			if _, ok := fs["entries"].([]any); !ok {
				return "", errors.New("destination native sandbox entries are malformed")
			}
		}
	case "external":
		if m["network"] != "restricted" && m["network"] != "enabled" {
			return "", errors.New("destination external sandbox policy is malformed")
		}
	default:
		return "", errors.New("destination legacy sandbox policy requires native normalization")
	}
	if original != "" {
		return original, nil
	}
	raw, err := json.Marshal(m)
	return string(raw), err
}

func nativeSameRow(rows []map[string]any, expected map[string]any) bool {
	if expected == nil {
		return len(rows) == 0
	}
	if len(rows) != 1 {
		return false
	}
	a, err := json.Marshal(rows[0])
	if err != nil {
		return false
	}
	b, err := json.Marshal(expected)
	return err == nil && bytes.Equal(a, b)
}

func nativeMapCWD(cwd, sourceCodexHome, targetCodexHome string) string {
	sourceUserHome := filepath.Dir(sourceCodexHome)
	targetUserHome := filepath.Dir(targetCodexHome)
	if filepath.Base(sourceCodexHome) == ".codex" && filepath.Base(targetCodexHome) == ".codex" && insidePath(sourceUserHome, cwd) {
		rel, err := filepath.Rel(sourceUserHome, cwd)
		if err == nil {
			return filepath.Join(targetUserHome, rel)
		}
	}
	return cwd
}
