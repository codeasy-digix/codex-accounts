package historysync

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// MaxBundleBytes bounds the entire decoded JSON bundle, including its base64
// rollout. Wire messages are independently bounded before JSON decoding.
const MaxBundleBytes int64 = 512 << 20
const maxWireBytes int64 = ((MaxBundleBytes + (1 << 20)) * 4 / 3) + (64 << 10)
const maxReportErrors = 100
const maxCycleLogBytes = 2 << 20

type Head struct {
	Entry        Entry  `json:"entry"`
	Node         string `json:"node"`
	ObjectDigest string `json:"object_digest,omitempty"`
}

// Conflicts are durable observations, including resolutions by event time.
// A later successful cycle never erases these records or the losing objects.
type Conflict struct {
	Key            string    `json:"key"`
	ID             string    `json:"id"`
	CurrentDigest  string    `json:"current_digest"`
	IncomingDigest string    `json:"incoming_digest"`
	WinnerDigest   string    `json:"winner_digest"`
	CurrentNode    string    `json:"current_node"`
	IncomingNode   string    `json:"incoming_node"`
	Reason         string    `json:"reason"`
	Revision       bool      `json:"revision,omitempty"`
	Resolved       bool      `json:"resolved"`
	Resolution     string    `json:"resolution,omitempty"`
	Observed       time.Time `json:"observed"`
}

type HubState struct {
	Heads          int        `json:"heads"`
	Objects        int        `json:"objects"`
	Conflicts      []Conflict `json:"conflicts"`
	ConflictsTotal int        `json:"conflicts_total"`
	RevisionsTotal int        `json:"revisions_total"`
}

type PutResult struct {
	Head     Head `json:"head"`
	Conflict bool `json:"conflict"`
}

type relayHub interface {
	Heads(context.Context) (map[string]Head, error)
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, []byte) (PutResult, error)
	State(context.Context) (HubState, error)
}

type Relay struct {
	cfg    Config
	native Native
	hub    relayHub
}

func NormalizeConfig(cfg Config) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg, err
	}
	if cfg.Node == "" {
		cfg.Node, err = os.Hostname()
		if err != nil {
			return cfg, err
		}
	}
	if !safeLabel(cfg.Node) {
		return cfg, errors.New("invalid node name")
	}
	if cfg.Home == "" {
		cfg.Home = filepath.Join(home, ".codex")
	}
	if cfg.Store == "" {
		cfg.Store = filepath.Join(home, ".codex-history-sync", "v2")
	}
	cfg.Home = expandHome(cfg.Home, home)
	cfg.Store = expandHome(cfg.Store, home)
	if !filepath.IsAbs(cfg.Home) || !filepath.IsAbs(cfg.Store) {
		return cfg, errors.New("home and store must be absolute paths")
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 120
	}
	if cfg.HubSSH == "" {
		if cfg.HubStore == "" {
			cfg.HubStore = filepath.Join(cfg.Store, "hub")
		}
		cfg.HubStore = expandHome(cfg.HubStore, home)
		if !filepath.IsAbs(cfg.HubStore) {
			return cfg, errors.New("local hub_store must be absolute")
		}
	} else {
		if !validSSHHost(cfg.HubSSH) {
			return cfg, errors.New("invalid SSH hub host")
		}
		if cfg.HubStore == "" {
			cfg.HubStore = "~/.codex-history-sync/v2/hub"
		}
		if cfg.HubBinary == "" {
			cfg.HubBinary = "~/.local/bin/codex-history-sync"
		}
		if !validRemotePath(cfg.HubStore) || !validRemotePath(cfg.HubBinary) {
			return cfg, errors.New("remote paths must be absolute or start with ~/")
		}
	}
	return cfg, nil
}

func NewRelay(cfg Config, native Native) (*Relay, error) {
	if native == nil {
		return nil, errors.New("native adapter is required")
	}
	cfg, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	r := &Relay{cfg: cfg, native: native}
	if cfg.HubSSH == "" {
		r.hub = &fileHub{store: cfg.HubStore}
	} else {
		r.hub = &sshHub{cfg: cfg}
	}
	return r, nil
}

// Sync serializes cycles on this device. Divergent local candidates are retained
// at the hub even when older. The chosen head always keeps its stable ID.
func (r *Relay) Sync(ctx context.Context, dryRun bool) (report CycleReport, resultErr error) {
	report = CycleReport{Started: time.Now().UTC(), Node: r.cfg.Node}
	if !dryRun && !r.cfg.Enabled {
		return report, errors.New("history synchronization is disabled in configuration")
	}
	var unlock func()
	if !dryRun {
		var err error
		unlock, err = acquireLock(ctx, r.cfg.Store, "cycle.lock")
		if err != nil {
			return report, err
		}
		defer unlock()
		if previous, err := ReadStatus(r.cfg.Store); err == nil {
			// Preserve the last durable total if this cycle is interrupted before
			// the hub can be refreshed. HubStateCurrent remains false until then.
			report.ConflictsTotal = previous.ConflictsTotal
		}
		defer func() {
			report.Finished = time.Now().UTC()
			report.Interrupted = ctx.Err() != nil
			// Local metadata remains readable after context cancellation. Refresh
			// it on every exit, including an interrupted upload/download loop.
			if count, err := ReadPendingCount(r.cfg.Store); err != nil {
				if len(report.Errors) < maxReportErrors {
					report.Errors = append(report.Errors, fmt.Sprintf("pending state: %v", err))
				}
				resultErr = errors.Join(resultErr, err)
			} else {
				report.Pending = count
			}
			if err := saveReport(r.cfg.Store, report); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
	} else {
		defer func() { report.Finished = time.Now().UTC(); report.Interrupted = ctx.Err() != nil }()
	}
	if hub, ok := r.hub.(*sshHub); ok {
		if err := hub.begin(ctx); err != nil {
			return report, err
		}
		defer hub.close()
	}
	addError := func(id string, err error) {
		if len(report.Errors) < maxReportErrors {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", id, err))
		}
	}
	conflictedIDs := map[string]bool{}
	addConflict := func(id string) {
		if !conflictedIDs[id] {
			conflictedIDs[id] = true
			report.Conflicts++
		}
	}
	receipts := map[string]bool{}
	receiptFile := filepath.Join(r.cfg.Store, "uploaded.json")
	if err := readOptionalJSON(receiptFile, &receipts, 64<<20); err != nil {
		return report, err
	}
	local, err := r.native.Inventory(ctx)
	if err != nil {
		addError("inventory", err)
		return report, err
	}
	heads, err := r.hub.Heads(ctx)
	if err != nil {
		addError("hub heads", err)
		return report, err
	}
	ids := sortedEntryIDs(local)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		entry := local[id]
		if !safeID(id) || entry.ID != id {
			addError(id, errors.New("invalid inventory ID"))
			continue
		}
		if entry.Error != "" {
			addError(id, errors.New(entry.Error))
			continue
		}
		if entry.Busy {
			report.Busy++
			continue
		}
		if head, ok := heads[id]; ok && ((head.Entry.Digest == entry.Digest && head.Entry.DependencyReason == entry.DependencyReason && entry.Digest != "") || (receipts[r.receiptKey(entry)] && head.Entry.LastEventNS >= entry.LastEventNS && !improvesHead(head, entry))) {
			report.Unchanged++
			continue
		}
		if dryRun {
			report.Uploaded++
			continue
		}
		bundle, err := r.native.Export(ctx, id)
		if err != nil {
			addError(id, err)
			continue
		}
		if bundle == nil || bundle.Entry.ID != id {
			addError(id, errors.New("export returned a different ID"))
			continue
		}
		if bundle.Entry.Busy {
			report.Busy++
			continue
		}
		bundle.Node = r.cfg.Node
		if head, exists := heads[id]; exists && receipts[r.receiptKey(bundle.Entry)] && head.Entry.LastEventNS >= bundle.Entry.LastEventNS && !improvesHead(head, bundle.Entry) {
			report.Unchanged++
			local[id] = bundle.Entry
			continue
		}
		data, err := encodeBundle(bundle)
		if err != nil {
			addError(id, err)
			continue
		}
		put, err := r.hub.Put(ctx, data)
		if err != nil {
			addError(id, err)
			continue
		}
		report.Uploaded++
		receipts[r.receiptKey(bundle.Entry)] = true
		if err := atomicJSON(receiptFile, receipts); err != nil {
			addError(id, err)
		}
		if put.Conflict {
			addConflict(id)
		}
		heads[id] = put.Head
		local[id] = bundle.Entry
	}
	if !dryRun {
		// Another device may have published while this device uploaded.
		heads, err = r.hub.Heads(ctx)
		if err != nil {
			addError("hub heads", err)
			return report, err
		}
	}
	existingPending := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(r.cfg.Store, "pending.json"), &existingPending, 16<<20); err != nil {
		addError("pending state", err)
		return report, err
	}
	for _, id := range sortedHeadIDs(heads) {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		head := heads[id]
		entry, exists := local[id]
		_, hasPending := existingPending[id]
		if exists && entry.Error == "" && entry.Digest == head.Entry.Digest && entry.DependencyReason == head.Entry.DependencyReason && !hasPending {
			if !dryRun {
				if err := r.clearPending(ctx, id, objectDigest(head)); err != nil {
					addError(id, err)
				}
			}
			continue
		}
		if exists && entry.LastEventNS > head.Entry.LastEventNS && !entry.Busy {
			continue
		}
		if dryRun {
			report.Downloaded++
			if entry.Busy {
				report.Deferred++
			}
			continue
		}
		bundle, cachedErr := r.cachedPending(head)
		if cachedErr == nil {
			report.Cached++
		} else {
			data, err := r.hub.Get(ctx, objectDigest(head))
			if err != nil {
				addError(id, err)
				continue
			}
			bundle, err = decodeHeadBundle(head, data)
			if err != nil {
				addError(id, err)
				continue
			}
			if err := r.stagePending(ctx, head, data); err != nil {
				addError(id, err)
				continue
			}
			report.Downloaded++
		}
		if exists && entry.Busy {
			report.Deferred++
			report.Results = append(report.Results, InstallResult{ID: id, Status: "deferred", Reason: "native thread is busy", Digest: head.Entry.Digest})
			continue
		}
		install, err := r.native.Install(ctx, bundle)
		if err != nil {
			report.Deferred++
			addError(id, err)
			continue
		}
		report.Results = append(report.Results, install)
		if install.Status == "deferred" || install.Status == "busy" || install.Status == "conflict" {
			report.Deferred++
			if install.Status == "conflict" {
				addConflict(id)
			}
			continue
		}
		if install.Status == "installed" {
			report.Installed++
		}
		if install.Status == "newer-local" {
			report.SkippedLocal++
		}
		if install.Status == "installed" || install.Status == "unchanged" || install.Status == "newer-local" {
			if err := r.clearPending(ctx, id, objectDigest(head)); err != nil {
				addError(id, err)
			}
		} else {
			report.Deferred++
			addError(id, fmt.Errorf("unrecognized install status %q", install.Status))
		}
	}
	state, err := r.hub.State(ctx)
	if err != nil {
		addError("hub state", err)
	} else {
		report.ConflictsTotal = state.ConflictsTotal
		report.HubStateCurrent = true
	}
	if len(report.Errors) > 0 {
		return report, fmt.Errorf("history cycle had %d errors (see last-report.json)", len(report.Errors))
	}
	return report, nil
}

func (r *Relay) HubState(ctx context.Context) (HubState, error) { return r.hub.State(ctx) }

// ReadHubState needs no native adapter and performs no native writes.
func ReadHubState(ctx context.Context, cfg Config) (HubState, error) {
	cfg, err := NormalizeConfig(cfg)
	if err != nil {
		return HubState{}, err
	}
	if cfg.HubSSH == "" {
		return (&fileHub{store: cfg.HubStore}).State(ctx)
	}
	return (&sshHub{cfg: cfg}).State(ctx)
}

func (r *Relay) receiptKey(entry Entry) string {
	identity := r.cfg.HubSSH + "\x00" + r.cfg.HubStore + "\x00" + r.cfg.HubBinary
	digest := sha256.Sum256([]byte(identity + "\x00" + entry.ID + "\x00" + entry.Digest + "\x00" + entry.DependencyReason))
	return hex.EncodeToString(digest[:])
}

func objectDigest(head Head) string {
	if head.ObjectDigest != "" {
		return head.ObjectDigest
	}
	return head.Entry.Digest // compatibility with raw-hash indexes
}

func improvesHead(head Head, entry Entry) bool {
	return head.Entry.Digest == entry.Digest && head.Entry.DependencyReason != "" && entry.DependencyReason == ""
}

func bundleObjectDigest(bundle *Bundle) (string, error) {
	raw, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func decodeHeadBundle(head Head, data []byte) (*Bundle, error) {
	bundle, err := decodeBundle(data)
	if err != nil {
		return nil, err
	}
	if bundle.Entry != head.Entry || bundle.Node != head.Node {
		return nil, errors.New("hub object does not match its head")
	}
	if head.ObjectDigest != "" {
		digest, err := bundleObjectDigest(bundle)
		if err != nil || digest != head.ObjectDigest {
			return nil, errors.New("whole bundle object digest mismatch")
		}
	}
	return bundle, nil
}

func (r *Relay) cachedPending(head Head) (*Bundle, error) {
	pending := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(r.cfg.Store, "pending.json"), &pending, 16<<20); err != nil {
		return nil, err
	}
	if old, ok := pending[head.Entry.ID]; !ok || objectDigest(old) != objectDigest(head) {
		return nil, os.ErrNotExist
	}
	data, err := readBounded(filepath.Join(r.cfg.Store, "pending", objectDigest(head)+".json.gz"), MaxBundleBytes+(1<<20))
	if err != nil {
		return nil, err
	}
	return decodeHeadBundle(head, data)
}

func ReadStatus(store string) (*CycleReport, error) {
	var report CycleReport
	if err := readJSON(filepath.Join(store, "last-report.json"), &report, 2<<20); err != nil {
		return nil, err
	}
	return &report, nil
}

func ReadPendingCount(store string) (int, error) {
	pending := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(store, "pending.json"), &pending, 16<<20); err != nil {
		return 0, err
	}
	return len(pending), nil
}

func (r *Relay) stagePending(ctx context.Context, head Head, data []byte) error {
	unlock, err := acquireLock(ctx, r.cfg.Store, "pending.lock")
	if err != nil {
		return err
	}
	defer unlock()
	dir := filepath.Join(r.cfg.Store, "pending")
	if err := privateDir(dir); err != nil {
		return err
	}
	name := filepath.Join(dir, objectDigest(head)+".json.gz")
	// Replacing a damaged cache never changes a published immutable hub object.
	if old, err := readBounded(name, MaxBundleBytes+(1<<20)); err == nil {
		if _, err := decodeHeadBundle(head, old); err != nil {
			if err := atomicWrite(name, data); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := atomicWrite(name, data); err != nil {
		return err
	}
	pending := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(r.cfg.Store, "pending.json"), &pending, 16<<20); err != nil {
		return err
	}
	pending[head.Entry.ID] = head
	return atomicJSON(filepath.Join(r.cfg.Store, "pending.json"), pending)
}

func (r *Relay) clearPending(ctx context.Context, id, digest string) error {
	unlock, err := acquireLock(ctx, r.cfg.Store, "pending.lock")
	if err != nil {
		return err
	}
	defer unlock()
	pending := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(r.cfg.Store, "pending.json"), &pending, 16<<20); err != nil {
		return err
	}
	if head, ok := pending[id]; ok && objectDigest(head) == digest {
		delete(pending, id)
		return atomicJSON(filepath.Join(r.cfg.Store, "pending.json"), pending)
	}
	return nil
}

type fileHub struct{ store string }

func (h *fileHub) Heads(ctx context.Context) (map[string]Head, error) {
	// A missing hub has no heads. Read-only plans must not create it.
	if _, err := os.Stat(h.store); errors.Is(err, os.ErrNotExist) {
		return map[string]Head{}, nil
	} else if err != nil {
		return nil, err
	}
	unlock, err := acquireLock(ctx, h.store, "hub.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	return h.readHeads()
}

func (h *fileHub) readHeads() (map[string]Head, error) {
	heads := map[string]Head{}
	if err := readOptionalJSON(filepath.Join(h.store, "heads.json"), &heads, 16<<20); err != nil {
		return nil, err
	}
	for id, head := range heads {
		if !safeID(id) || head.Entry.ID != id || !validDigest(head.Entry.Digest) || (head.ObjectDigest != "" && !validDigest(head.ObjectDigest)) || head.Entry.LastEventNS < 0 || !safeLabel(head.Node) {
			return nil, errors.New("invalid hub head index")
		}
	}
	return heads, nil
}

func (h *fileHub) Get(ctx context.Context, digest string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDigest(digest) {
		return nil, errors.New("invalid object digest")
	}
	return readBounded(filepath.Join(h.store, "objects", digest+".json.gz"), MaxBundleBytes+(1<<20))
}

func (h *fileHub) Put(ctx context.Context, data []byte) (PutResult, error) {
	bundle, err := decodeBundle(data)
	if err != nil {
		return PutResult{}, err
	}
	unlock, err := acquireLock(ctx, h.store, "hub.lock")
	if err != nil {
		return PutResult{}, err
	}
	defer unlock()
	if err := privateDir(filepath.Join(h.store, "objects")); err != nil {
		return PutResult{}, err
	}
	heads, err := h.readHeads()
	if err != nil {
		return PutResult{}, err
	}
	objectHash, err := bundleObjectDigest(bundle)
	if err != nil {
		return PutResult{}, err
	}
	incoming := Head{Entry: bundle.Entry, Node: bundle.Node, ObjectDigest: objectHash}
	objectName := filepath.Join(h.store, "objects", objectHash+".json.gz")
	if old, err := readBounded(objectName, MaxBundleBytes+(1<<20)); err == nil {
		if _, err := decodeHeadBundle(incoming, old); err != nil {
			return PutResult{}, fmt.Errorf("existing immutable object is damaged: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return PutResult{}, err
	}
	if err := immutableWrite(objectName, data); err != nil {
		return PutResult{}, err
	}
	incoming.Entry.Busy, incoming.Entry.Error = false, ""
	current, exists := heads[bundle.Entry.ID]
	if exists && current.Entry.Digest == incoming.Entry.Digest {
		if current.Entry.DependencyReason != "" && incoming.Entry.DependencyReason == "" {
			heads[bundle.Entry.ID] = incoming
			if err := atomicJSON(filepath.Join(h.store, "heads.json"), heads); err != nil {
				return PutResult{}, err
			}
			return PutResult{Head: incoming}, nil
		}
		return PutResult{Head: current}, nil
	}
	winner := incoming
	conflicted := exists
	if exists {
		revision := false
		oldData, err := h.Get(ctx, objectDigest(current))
		if err != nil {
			return PutResult{}, err
		}
		oldBundle, err := decodeHeadBundle(current, oldData)
		if err != nil {
			return PutResult{}, err
		}
		revision = bytes.HasPrefix(bundle.Raw, oldBundle.Raw) || bytes.HasPrefix(oldBundle.Raw, bundle.Raw)
		conflicted = !revision
		reason := "incoming_event_newer"
		if incoming.Entry.LastEventNS < current.Entry.LastEventNS {
			winner, reason = current, "incoming_event_older"
		}
		if incoming.Entry.LastEventNS == current.Entry.LastEventNS {
			winner, reason = current, "equal_event_time_keep_current"
		}
		if revision {
			reason = "append_only_revision"
		}
		keyBytes := sha256.Sum256([]byte(bundle.Entry.ID + "\x00" + current.Entry.Digest + "\x00" + incoming.Entry.Digest + "\x00" + winner.Entry.Digest))
		key := hex.EncodeToString(keyBytes[:])
		ledger := map[string]Conflict{}
		if err := readOptionalJSON(filepath.Join(h.store, "conflicts.json"), &ledger, 64<<20); err != nil {
			return PutResult{}, err
		}
		if _, recorded := ledger[key]; !recorded {
			resolution := "newer_event_selected"
			resolved := incoming.Entry.LastEventNS != current.Entry.LastEventNS || revision
			if !resolved {
				resolution = "equal_event_time_requires_review"
			}
			ledger[key] = Conflict{Key: key, ID: bundle.Entry.ID, CurrentDigest: current.Entry.Digest, IncomingDigest: incoming.Entry.Digest, WinnerDigest: winner.Entry.Digest, CurrentNode: current.Node, IncomingNode: incoming.Node, Reason: reason, Revision: revision, Resolved: resolved, Resolution: resolution, Observed: time.Now().UTC()}
			if err := atomicJSON(filepath.Join(h.store, "conflicts.json"), ledger); err != nil {
				return PutResult{}, err
			}
		}
	}
	heads[bundle.Entry.ID] = winner
	if err := atomicJSON(filepath.Join(h.store, "heads.json"), heads); err != nil {
		return PutResult{}, err
	}
	return PutResult{Head: winner, Conflict: conflicted}, nil
}

func (h *fileHub) State(ctx context.Context) (HubState, error) {
	if _, err := os.Stat(h.store); errors.Is(err, os.ErrNotExist) {
		return HubState{Conflicts: []Conflict{}}, nil
	} else if err != nil {
		return HubState{}, err
	}
	unlock, err := acquireLock(ctx, h.store, "hub.lock")
	if err != nil {
		return HubState{}, err
	}
	defer unlock()
	heads, err := h.readHeads()
	if err != nil {
		return HubState{}, err
	}
	ledger := map[string]Conflict{}
	if err := readOptionalJSON(filepath.Join(h.store, "conflicts.json"), &ledger, 64<<20); err != nil {
		return HubState{}, err
	}
	state := HubState{Heads: len(heads), Conflicts: []Conflict{}}
	for _, c := range ledger {
		state.Conflicts = append(state.Conflicts, c)
		if c.Revision {
			state.RevisionsTotal++
		} else {
			state.ConflictsTotal++
		}
	}
	sort.Slice(state.Conflicts, func(i, j int) bool { return state.Conflicts[i].Key < state.Conflicts[j].Key })
	objects, err := os.ReadDir(filepath.Join(h.store, "objects"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	for _, file := range objects {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".json.gz") {
			state.Objects++
		}
	}
	return state, nil
}

type rpcRequest struct {
	Op     string `json:"op"`
	Digest string `json:"digest,omitempty"`
	Data   []byte `json:"data,omitempty"`
}
type rpcResponse struct {
	Error string          `json:"error,omitempty"`
	Heads map[string]Head `json:"heads,omitempty"`
	Data  []byte          `json:"data,omitempty"`
	Put   *PutResult      `json:"put,omitempty"`
	State *HubState       `json:"state,omitempty"`
}

// Serve is a narrow newline-JSON protocol. It receives only conversation
// bundles; it has no operations for native state, credentials, or commands.
func Serve(ctx context.Context, store string, input io.Reader, output io.Writer) error {
	hub := &fileHub{store: store}
	reader := bufio.NewReader(input)
	encoder := json.NewEncoder(output)
	for {
		line, err := readLineBounded(reader, maxWireBytes)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return errors.New("incomplete relay request")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		var request rpcRequest
		response := rpcResponse{}
		if err := decodeStrict(line, &request); err != nil {
			response.Error = "invalid relay request"
		} else {
			switch request.Op {
			case "heads":
				response.Heads, err = hub.Heads(ctx)
			case "get":
				response.Data, err = hub.Get(ctx, request.Digest)
			case "put":
				var put PutResult
				put, err = hub.Put(ctx, request.Data)
				if err == nil {
					response.Put = &put
				}
			case "state":
				var state HubState
				state, err = hub.State(ctx)
				if err == nil {
					response.State = &state
				}
			default:
				err = errors.New("unsupported relay operation")
			}
			if err != nil {
				response.Error = err.Error()
			}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

type sshHub struct {
	cfg      Config
	mu       sync.Mutex
	cycleCtx context.Context
	session  *sshSession
}

type sshSession struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	cancel context.CancelFunc
}

func (h *sshHub) begin(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cycleCtx != nil {
		return errors.New("SSH relay cycle already open")
	}
	h.cycleCtx = ctx
	return nil
}

func (h *sshHub) closeSession() {
	if h.session != nil {
		_ = h.session.input.Close()
		h.session.cancel()
		_ = h.session.cmd.Wait()
		h.session = nil
	}
}

func (h *sshHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeSession()
	h.cycleCtx = nil
}

func (h *sshHub) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	command := "exec " + quoteRemotePath(h.cfg.HubBinary) + " serve --store " + quoteRemotePath(h.cfg.HubStore)
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "--", h.cfg.HubSSH, command)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return err
	}
	stderr := &limitedBuffer{max: 16 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		input.Close()
		cancel()
		return fmt.Errorf("SSH relay failed: %w", err)
	}
	h.session = &sshSession{cmd: cmd, input: input, output: bufio.NewReader(output), cancel: cancel}
	return nil
}

func (h *sshHub) call(ctx context.Context, request rpcRequest) (rpcResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	oneShot := h.cycleCtx == nil
	if h.session == nil {
		sessionCtx := h.cycleCtx
		if sessionCtx == nil {
			sessionCtx = ctx
		}
		if err := h.start(sessionCtx); err != nil {
			return rpcResponse{}, err
		}
	}
	if oneShot {
		defer h.closeSession()
	}
	session := h.session
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	type result struct {
		response rpcResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		limit := &countWriter{target: session.input, max: maxWireBytes}
		if err := json.NewEncoder(limit).Encode(request); err != nil {
			done <- result{err: err}
			return
		}
		line, err := readLineBounded(session.output, maxWireBytes)
		if err != nil {
			done <- result{err: fmt.Errorf("incomplete SSH relay response: %w", err)}
			return
		}
		var response rpcResponse
		if err := decodeStrict(line, &response); err != nil {
			done <- result{err: errors.New("invalid SSH relay response")}
			return
		}
		if response.Error != "" {
			done <- result{response: response, err: errors.New(response.Error)}
			return
		}
		done <- result{response: response}
	}()
	select {
	case <-callCtx.Done():
		h.closeSession()
		<-done
		return rpcResponse{}, callCtx.Err()
	case result := <-done:
		if result.err != nil {
			h.closeSession()
		}
		return result.response, result.err
	}
}

func (h *sshHub) Heads(ctx context.Context) (map[string]Head, error) {
	r, e := h.call(ctx, rpcRequest{Op: "heads"})
	if r.Heads == nil {
		r.Heads = map[string]Head{}
	}
	return r.Heads, e
}
func (h *sshHub) Get(ctx context.Context, digest string) ([]byte, error) {
	r, e := h.call(ctx, rpcRequest{Op: "get", Digest: digest})
	return r.Data, e
}
func (h *sshHub) Put(ctx context.Context, data []byte) (PutResult, error) {
	r, e := h.call(ctx, rpcRequest{Op: "put", Data: data})
	if e != nil {
		return PutResult{}, e
	}
	if r.Put == nil {
		return PutResult{}, errors.New("missing relay put result")
	}
	return *r.Put, nil
}
func (h *sshHub) State(ctx context.Context) (HubState, error) {
	r, e := h.call(ctx, rpcRequest{Op: "state"})
	if e != nil {
		return HubState{}, e
	}
	if r.State == nil {
		return HubState{}, errors.New("missing relay state")
	}
	return *r.State, nil
}

func encodeBundle(bundle *Bundle) ([]byte, error) {
	if err := validateBundle(bundle); err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	limit := &countWriter{target: writer, max: MaxBundleBytes}
	if err := json.NewEncoder(limit).Encode(bundle); err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func decodeBundle(data []byte) (*Bundle, error) {
	if int64(len(data)) > MaxBundleBytes+(1<<20) {
		return nil, errors.New("compressed bundle exceeds size limit")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid gzip bundle")
	}
	defer reader.Close()
	// Decode directly from the inflater rather than retaining another complete
	// inflated JSON copy alongside the decoder buffer, raw history and projection.
	limit := &io.LimitedReader{R: reader, N: MaxBundleBytes + 1}
	var bundle Bundle
	err = decodeStrictReader(limit, &bundle)
	if limit.N == 0 {
		return nil, errors.New("decoded bundle exceeds size limit")
	}
	if err != nil {
		return nil, errors.New("invalid bundle JSON")
	}
	if err := validateBundle(&bundle); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func validateBundle(bundle *Bundle) error {
	if bundle == nil || bundle.Format != Format {
		return errors.New("unsupported bundle format")
	}
	if !safeID(bundle.Entry.ID) || !safeLabel(bundle.Node) {
		return errors.New("invalid bundle identity")
	}
	if bundle.Entry.Busy || bundle.Entry.Error != "" {
		return errors.New("busy or failed exports cannot be published")
	}
	if bundle.Entry.LastEventNS < 0 || len(bundle.Raw) == 0 || int64(len(bundle.Raw)) > MaxBundleBytes {
		return errors.New("invalid rollout size or event time")
	}
	info, err := inspectNativeRaw(bundle.Raw, bundle.Entry.ID)
	if err != nil {
		return err
	}
	if info.lastNS != bundle.Entry.LastEventNS {
		return errors.New("bundle event time disagrees with durable rollout")
	}
	digest := sha256.Sum256(bundle.Raw)
	if bundle.Entry.Digest != hex.EncodeToString(digest[:]) {
		return errors.New("bundle rollout digest mismatch")
	}
	if id, exists := bundle.Row["id"]; exists && id != bundle.Entry.ID {
		return errors.New("bundle row ID mismatch")
	}
	if _, err := validateNativeBundle(bundle); err != nil {
		return err
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	return decodeStrictReader(bytes.NewReader(data), target)
}

func decodeStrictReader(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("extra JSON after message")
	}
	return nil
}

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.Chmod(dir, 0700)
}

func acquireLock(ctx context.Context, dir, name string) (func(), error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func atomicJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(name, append(data, '\n'))
}
func atomicWrite(name string, data []byte) error {
	dir := filepath.Dir(name)
	if err := privateDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".relay-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, name); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func immutableWrite(name string, data []byte) error {
	if _, err := os.Stat(name); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// All callers hold the hub or pending lock across the existence check.
	return atomicWrite(name, data)
}

func readBounded(name string, max int64) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("relay file exceeds size limit")
	}
	return data, nil
}
func readJSON(name string, value any, max int64) error {
	data, err := readBounded(name, max)
	if err != nil {
		return err
	}
	return decodeStrict(data, value)
}
func readOptionalJSON(name string, value any, max int64) error {
	err := readJSON(name, value, max)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func saveReport(store string, report CycleReport) error {
	if err := atomicJSON(filepath.Join(store, "last-report.json"), report); err != nil {
		return err
	}
	line, err := json.Marshal(report)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	name := filepath.Join(store, "cycles.log")
	old, err := readBounded(name, maxCycleLogBytes)
	if errors.Is(err, os.ErrNotExist) {
		old = nil
	} else if err != nil {
		return err
	}
	if len(old)+len(line) > maxCycleLogBytes {
		old = old[len(old)/2:]
		if newline := bytes.IndexByte(old, '\n'); newline >= 0 {
			old = old[newline+1:]
		} else {
			old = nil
		}
	}
	return atomicWrite(name, append(old, line...))
}

func readLineBounded(reader *bufio.Reader, max int64) ([]byte, error) {
	var line bytes.Buffer
	for {
		part, err := reader.ReadSlice('\n')
		if int64(line.Len())+int64(len(part)) > max {
			return nil, errors.New("relay message exceeds size limit")
		}
		line.Write(part)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line.Bytes(), err
	}
}

type limitedBuffer struct {
	bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if int64(b.Len())+int64(len(data)) > b.max {
		return 0, errors.New("relay response exceeds size limit")
	}
	return b.Buffer.Write(data)
}

type countWriter struct {
	target io.Writer
	n, max int64
}

func (w *countWriter) Write(data []byte) (int, error) {
	if w.n+int64(len(data)) > w.max {
		return 0, errors.New("decoded bundle exceeds size limit")
	}
	n, err := w.target.Write(data)
	w.n += int64(n)
	return n, err
}

func validDigest(digest string) bool {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
func safeID(id string) bool {
	return safeLabel(id) && !strings.ContainsAny(id, "/\\") && id != "." && id != ".."
}
func safeLabel(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func expandHome(value, home string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	return filepath.Clean(value)
}
func validSSHHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.@:-[]", r)) {
			return false
		}
	}
	return true
}
func validRemotePath(value string) bool {
	return (strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/")) && !strings.ContainsAny(value, "\x00\r\n")
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func quoteRemotePath(value string) string {
	if strings.HasPrefix(value, "~/") {
		return "\"$HOME\"/" + shellQuote(value[2:])
	}
	return shellQuote(value)
}
func sortedEntryIDs(entries map[string]Entry) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func sortedHeadIDs(heads map[string]Head) []string {
	ids := make([]string, 0, len(heads))
	for id := range heads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
