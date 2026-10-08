package historysync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const relayTestID = "01a0f156-d4b2-72a2-8a45-9834150686c9"

func relayFixture(t *testing.T, node string, timestamp int, text string) *Bundle {
	t.Helper()
	raw := []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":\"2026-10-01T00:00:00Z\",\"payload\":{\"id\":%q}}\n{\"type\":\"event_msg\",\"timestamp\":\"2026-10-01T00:00:%02dZ\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n", relayTestID, timestamp, text))
	info, err := inspectNativeRaw(raw, relayTestID)
	if err != nil {
		t.Fatal(err)
	}
	return &Bundle{Format: Format, Node: node, SourceHome: "/fixture/" + node, Entry: Entry{ID: relayTestID, RolloutID: relayTestID, Digest: info.digest, LastEventNS: info.lastNS}, Raw: raw, Row: map[string]any{"id": relayTestID, "history_mode": "legacy", "archived": json.Number("0"), "updated_at": json.Number("9007199254740993")}, History: map[string][]map[string]any{}}
}

type relayFakeNative struct {
	bundles  map[string]*Bundle
	busy     bool
	installs int
}

func (n *relayFakeNative) Inventory(context.Context) (map[string]Entry, error) {
	entries := map[string]Entry{}
	for id, b := range n.bundles {
		e := b.Entry
		e.Busy = n.busy
		entries[id] = e
	}
	return entries, nil
}
func (n *relayFakeNative) Export(_ context.Context, id string) (*Bundle, error) {
	b := *n.bundles[id]
	return &b, nil
}
func (n *relayFakeNative) Install(_ context.Context, b *Bundle) (InstallResult, error) {
	result := InstallResult{ID: b.Entry.ID, Digest: b.Entry.Digest}
	if n.busy {
		result.Status = "deferred"
		return result, nil
	}
	if old := n.bundles[b.Entry.ID]; old != nil {
		if old.Entry.Digest == b.Entry.Digest {
			result.Status = "unchanged"
			return result, nil
		}
		if old.Entry.LastEventNS > b.Entry.LastEventNS {
			result.Status = "newer-local"
			return result, nil
		}
		if old.Entry.LastEventNS == b.Entry.LastEventNS {
			result.Status = "conflict"
			return result, nil
		}
	}
	n.bundles[b.Entry.ID] = b
	n.installs++
	result.Status = "installed"
	return result, nil
}

type relayCountingHub struct {
	relayHub
	puts, gets int
}

func (h *relayCountingHub) Put(ctx context.Context, data []byte) (PutResult, error) {
	h.puts++
	return h.relayHub.Put(ctx, data)
}
func (h *relayCountingHub) Get(ctx context.Context, digest string) ([]byte, error) {
	h.gets++
	return h.relayHub.Get(ctx, digest)
}
func newFakeRelay(t *testing.T, root, node, hubStore string, b *Bundle) (*Relay, *relayFakeNative, *relayCountingHub) {
	t.Helper()
	n := &relayFakeNative{bundles: map[string]*Bundle{}}
	if b != nil {
		n.bundles[b.Entry.ID] = b
	}
	r, err := NewRelay(Config{Node: node, Home: filepath.Join(root, node, "native"), Store: filepath.Join(root, node, "store"), HubStore: hubStore, Enabled: true}, n)
	if err != nil {
		t.Fatal(err)
	}
	h := &relayCountingHub{relayHub: r.hub}
	r.hub = h
	return r, n, h
}
func syncFake(t *testing.T, r *Relay) CycleReport {
	t.Helper()
	report, err := r.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v %+v", err, report)
	}
	return report
}

func TestThreeClientsLatestActualEventWinsAndLosingObjectsRemain(t *testing.T) {
	root := t.TempDir()
	hubStore := filepath.Join(root, "hub")
	a, an, _ := newFakeRelay(t, root, "a", hubStore, relayFixture(t, "a", 1, "a"))
	b, bn, _ := newFakeRelay(t, root, "b", hubStore, relayFixture(t, "b", 2, "b"))
	c, cn, _ := newFakeRelay(t, root, "c", hubStore, relayFixture(t, "c", 3, "c"))
	syncFake(t, a)
	syncFake(t, b)
	syncFake(t, c)
	syncFake(t, a)
	syncFake(t, b)
	winner := cn.bundles[relayTestID].Entry.Digest
	for _, n := range []*relayFakeNative{an, bn, cn} {
		if len(n.bundles) != 1 || n.bundles[relayTestID].Entry.Digest != winner {
			t.Fatal("stable UUID did not converge")
		}
	}
	state, err := (&fileHub{store: hubStore}).State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Objects != 3 || state.Heads != 1 || state.ConflictsTotal != 2 {
		t.Fatalf("missing preserved versions: %+v", state)
	}
	for _, conflict := range state.Conflicts {
		if !conflict.Resolved {
			t.Fatal("newer actual event should record resolution")
		}
	}
	report := syncFake(t, a)
	if report.Uploaded != 0 || report.Downloaded != 0 || report.ConflictsTotal != 2 {
		t.Fatalf("unchanged cycle lost durable conflict count: %+v", report)
	}
}

func TestPendingBusySecondCycleHasNoPayloadTransfers(t *testing.T) {
	root := t.TempDir()
	hubStore := filepath.Join(root, "hub")
	old := relayFixture(t, "a", 1, "older")
	a, an, ah := newFakeRelay(t, root, "a", hubStore, old)
	b, _, _ := newFakeRelay(t, root, "b", hubStore, relayFixture(t, "b", 2, "newer"))
	syncFake(t, a)
	syncFake(t, b)
	an.busy = true
	first := syncFake(t, a)
	gets, puts := ah.gets, ah.puts
	second := syncFake(t, a)
	if first.Pending != 1 || first.Downloaded != 1 || second.Pending != 1 || second.Cached != 1 || second.Downloaded != 0 || second.Uploaded != 0 || ah.gets != gets || ah.puts != puts {
		t.Fatalf("pending payload retransferred: first=%+v second=%+v", first, second)
	}
	an.busy = false
	final := syncFake(t, a)
	if final.Uploaded != 0 || final.Installed != 1 || final.Pending != 0 || ah.puts != puts || ah.gets != gets {
		t.Fatalf("losing local candidate reuploaded: %+v", final)
	}
}

func TestEqualTimeConflictStaysPending(t *testing.T) {
	root := t.TempDir()
	hubStore := filepath.Join(root, "hub")
	a, _, _ := newFakeRelay(t, root, "a", hubStore, relayFixture(t, "a", 2, "a"))
	b, _, bh := newFakeRelay(t, root, "b", hubStore, relayFixture(t, "b", 2, "b"))
	syncFake(t, a)
	first := syncFake(t, b)
	second := syncFake(t, b)
	if first.Pending != 1 || first.Deferred != 1 || second.Pending != 1 || second.Conflicts != 1 || second.Cached != 1 || second.Uploaded != 0 || bh.puts != 1 || bh.gets != 1 {
		t.Fatalf("equal-time conflict was discarded or retransferred: %+v %+v", first, second)
	}
}

func TestWholeBundleObjectsAndTimestampValidation(t *testing.T) {
	hub := &fileHub{store: filepath.Join(t.TempDir(), "hub")}
	ctx := context.Background()
	b := relayFixture(t, "a", 1, "a")
	data, err := encodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	first, err := hub.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	b.Row["cwd"] = "/different"
	data, err = encodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	state, err := hub.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Objects != 2 || first.Head.ObjectDigest == "" || second.Head.ObjectDigest != first.Head.ObjectDigest {
		t.Fatalf("same raw collapsed distinct bundles: %+v %+v", first, second)
	}
	decoded, err := decodeBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Row["updated_at"].(json.Number).String(); got != "9007199254740993" {
		t.Fatal("integer precision lost", got)
	}
	b.Entry.LastEventNS++
	if _, err := encodeBundle(b); err == nil {
		t.Fatal("client timestamp spoof accepted")
	}
}

func TestAppendOnlyRevisionsAreNotDivergenceConflicts(t *testing.T) {
	hub := &fileHub{store: filepath.Join(t.TempDir(), "hub")}
	ctx := context.Background()
	b := relayFixture(t, "a", 1, "a")
	data, err := encodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Put(ctx, data); err != nil {
		t.Fatal(err)
	}
	b.Raw = append(b.Raw, []byte("{\"type\":\"event_msg\",\"timestamp\":\"2026-10-01T00:00:02Z\",\"payload\":{\"type\":\"user_message\",\"message\":\"more\"}}\n")...)
	info, err := inspectNativeRaw(b.Raw, b.Entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	b.Entry.Digest = info.digest
	b.Entry.LastEventNS = info.lastNS
	data, err = encodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	put, err := hub.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	state, err := hub.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if put.Conflict || state.ConflictsTotal != 0 || state.RevisionsTotal != 1 || state.Objects != 2 {
		t.Fatalf("append history mislabeled divergence: %+v %+v", put, state)
	}
}

func TestHubConcurrentWritersRemainConsistent(t *testing.T) {
	store := filepath.Join(t.TempDir(), "hub")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 1; i <= 20; i++ {
		b := relayFixture(t, fmt.Sprintf("node%d", i), i, fmt.Sprintf("version%d", i))
		data, err := encodeBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(data []byte) { defer wg.Done(); _, err := (&fileHub{store: store}).Put(ctx, data); errs <- err }(data)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	heads, err := (&fileHub{store: store}).Heads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 1 || heads[relayTestID].Entry.LastEventNS != time.Date(2026, 10, 1, 0, 0, 20, 0, time.UTC).UnixNano() {
		t.Fatal("concurrent writes lost newest head", heads)
	}
	state, err := (&fileHub{store: store}).State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Objects != 20 {
		t.Fatal("concurrent version loss", state.Objects)
	}
	data, err := (&fileHub{store: store}).Get(ctx, heads[relayTestID].ObjectDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeHeadBundle(heads[relayTestID], data); err != nil {
		t.Fatal(err)
	}
}

func TestServeRejectsPartialWireAndAllowsDuplexMessages(t *testing.T) {
	store := filepath.Join(t.TempDir(), "hub")
	var output bytes.Buffer
	if err := Serve(context.Background(), store, bytes.NewBufferString("{\"op\":\"heads\"}"), &output); err == nil {
		t.Fatal("partial wire message accepted")
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatal("partial request mutated store", err)
	}
	output.Reset()
	if err := Serve(context.Background(), store, bytes.NewBufferString("{\"op\":\"heads\"}\n{\"op\":\"state\"}\n"), &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for i := 0; i < 2; i++ {
		var response rpcResponse
		if err := decoder.Decode(&response); err != nil || response.Error != "" {
			t.Fatal("duplex response", err, response)
		}
	}
}

func TestRelaySSHHelperProcess(t *testing.T) {
	store := os.Getenv("RELAY_SSH_HELPER_STORE")
	if store == "" {
		return
	}
	if err := Serve(context.Background(), store, os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSSHUsesOneDuplexProcessPerCycleAndClosesIt(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(root, "count")
	script := "#!/bin/sh\nprintf 'start\\n' >> \"$RELAY_SSH_COUNT\"\nexec " + shellQuote(os.Args[0]) + " -test.run=TestRelaySSHHelperProcess\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_SSH_COUNT", count)
	t.Setenv("RELAY_SSH_HELPER_STORE", filepath.Join(root, "hub"))
	n := &relayFakeNative{bundles: map[string]*Bundle{relayTestID: relayFixture(t, "a", 1, "a")}}
	r, err := NewRelay(Config{Node: "a", Home: filepath.Join(root, "native"), Store: filepath.Join(root, "client"), HubSSH: "fixture", HubStore: "/fixture/hub", HubBinary: "/fixture/bin", Enabled: true}, n)
	if err != nil {
		t.Fatal(err)
	}
	first := syncFake(t, r)
	second := syncFake(t, r)
	starts, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(starts, []byte("start\n")) != 2 || first.Uploaded != 1 || second.Uploaded != 0 {
		t.Fatalf("SSH processes reopened for each RPC: %q %+v %+v", starts, first, second)
	}
	if r.hub.(*sshHub).session != nil || r.hub.(*sshHub).cycleCtx != nil {
		t.Fatal("SSH cycle left a process open")
	}
}

func TestSameRawHealthyProjectionReplacesGatedHeadEvenWithOldReceipt(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "hub")
	bundle := relayFixture(t, "a", 1, "a")
	bundle.Entry.DependencyReason = "projection is behind"
	r, n, _ := newFakeRelay(t, root, "a", store, bundle)
	syncFake(t, r)
	hub := &fileHub{store: store}
	oldHeads, err := hub.Heads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldHead := oldHeads[relayTestID]
	n.bundles[relayTestID].Entry.DependencyReason = ""
	first := syncFake(t, r)
	heads, err := hub.Heads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Uploaded != 1 || heads[relayTestID].Entry.DependencyReason != "" || heads[relayTestID].ObjectDigest == oldHead.ObjectDigest {
		t.Fatalf("healthy same-raw projection suppressed: %+v %+v", first, heads)
	}
	// A restored older index must not let an earlier upload receipt poison healing.
	if err := atomicJSON(filepath.Join(store, "heads.json"), oldHeads); err != nil {
		t.Fatal(err)
	}
	second := syncFake(t, r)
	state, err := hub.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Uploaded != 1 || state.Objects != 2 || state.ConflictsTotal != 0 {
		t.Fatalf("receipt suppressed restored-index healing: %+v %+v", second, state)
	}
}

type relayCancelNative struct {
	*relayFakeNative
	cancel context.CancelFunc
}

func (n *relayCancelNative) Install(_ context.Context, b *Bundle) (InstallResult, error) {
	n.cancel()
	return InstallResult{ID: b.Entry.ID, Status: "deferred", Digest: b.Entry.Digest}, nil
}

func TestInterruptedCyclePreservesPendingAndKnownConflictCount(t *testing.T) {
	root := t.TempDir()
	hub := &fileHub{store: filepath.Join(root, "hub")}
	for i := 1; i <= 2; i++ {
		b := relayFixture(t, fmt.Sprintf("node%d", i), i, fmt.Sprintf("version%d", i))
		data, err := encodeBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Put(context.Background(), data); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	native := &relayCancelNative{relayFakeNative: &relayFakeNative{bundles: map[string]*Bundle{}}, cancel: cancel}
	store := filepath.Join(root, "client")
	if err := saveReport(store, CycleReport{Node: "client", ConflictsTotal: 1, HubStateCurrent: true}); err != nil {
		t.Fatal(err)
	}
	relay, err := NewRelay(Config{Node: "client", Home: filepath.Join(root, "native"), Store: store, HubStore: hub.store, Enabled: true}, native)
	if err != nil {
		t.Fatal(err)
	}
	report, err := relay.Sync(ctx, false)
	if err == nil || !report.Interrupted || report.Pending != 1 || report.Downloaded != 1 || report.Deferred != 1 || report.ConflictsTotal != 1 || report.HubStateCurrent {
		t.Fatalf("interrupted cycle reset counts: err=%v report=%+v", err, report)
	}
	saved, err := ReadStatus(store)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Interrupted || saved.Pending != 1 || saved.ConflictsTotal != 1 {
		t.Fatalf("saved interrupted report lost state: %+v", saved)
	}
}

type relayPausedNative struct {
	*relayFakeNative
	pause        string
	installCalls int
}

func (n *relayPausedNative) PublicationPauseReason(context.Context) string { return n.pause }
func (n *relayPausedNative) Install(ctx context.Context, b *Bundle) (InstallResult, error) {
	n.installCalls++
	if n.pause != "" {
		return InstallResult{ID: b.Entry.ID, Status: "deferred", Reason: n.pause, Digest: b.Entry.Digest}, nil
	}
	return n.relayFakeNative.Install(ctx, b)
}

func TestPausedCachedPendingSkipsDecodeAndResumesFullValidation(t *testing.T) {
	root := t.TempDir()
	hub := &fileHub{store: filepath.Join(root, "hub")}
	b := relayFixture(t, "remote", 1, "latest")
	data, err := encodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	put, err := hub.Put(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	n := &relayPausedNative{relayFakeNative: &relayFakeNative{bundles: map[string]*Bundle{}}, pause: "native Codex processes are running; publication deferred"}
	relay, err := NewRelay(Config{Node: "local", Home: filepath.Join(root, "native"), Store: filepath.Join(root, "client"), HubStore: hub.store, Enabled: true}, n)
	if err != nil {
		t.Fatal(err)
	}
	counter := &relayCountingHub{relayHub: relay.hub}
	relay.hub = counter
	first := syncFake(t, relay)
	if first.Downloaded != 1 || first.Pending != 1 || first.Deferred != 1 || n.installCalls != 1 {
		t.Fatalf("new download was not validated/staged: %+v calls=%d", first, n.installCalls)
	}
	cachedPath := filepath.Join(relay.cfg.Store, "pending", objectDigest(put.Head)+".json.gz")
	// If the paused path attempts to decode this damaged object it must fetch
	// again. A paused cycle should only check its exact head and file presence.
	if err := os.WriteFile(cachedPath, []byte("not a gzip bundle"), 0600); err != nil {
		t.Fatal(err)
	}
	paused := syncFake(t, relay)
	if paused.Cached != 1 || paused.Deferred != 1 || paused.Pending != 1 || paused.Downloaded != 0 || counter.gets != 1 || n.installCalls != 1 {
		t.Fatalf("paused cache was decoded: %+v gets=%d calls=%d", paused, counter.gets, n.installCalls)
	}
	n.pause = ""
	resumed := syncFake(t, relay)
	if resumed.Downloaded != 1 || resumed.Installed != 1 || resumed.Pending != 0 || counter.gets != 2 || n.installCalls != 2 {
		t.Fatalf("resume skipped full cache validation/install: %+v gets=%d calls=%d", resumed, counter.gets, n.installCalls)
	}
	if n.bundles[relayTestID].Entry.Digest != b.Entry.Digest {
		t.Fatal("damaged cache was installed")
	}
}

func TestPausedPendingStillReceivesNewHead(t *testing.T) {
	root := t.TempDir()
	hub := &fileHub{store: filepath.Join(root, "hub")}
	publish := func(b *Bundle) Head {
		t.Helper()
		data, err := encodeBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		put, err := hub.Put(context.Background(), data)
		if err != nil {
			t.Fatal(err)
		}
		return put.Head
	}
	publish(relayFixture(t, "remote", 1, "older"))
	n := &relayPausedNative{relayFakeNative: &relayFakeNative{bundles: map[string]*Bundle{}}, pause: "native process guard unavailable; publication deferred"}
	relay, err := NewRelay(Config{Node: "local", Home: filepath.Join(root, "native"), Store: filepath.Join(root, "client"), HubStore: hub.store, Enabled: true}, n)
	if err != nil {
		t.Fatal(err)
	}
	syncFake(t, relay)
	newer := publish(relayFixture(t, "remote", 2, "newer"))
	second := syncFake(t, relay)
	if second.Downloaded != 1 || second.Cached != 0 || second.Pending != 1 || second.Deferred != 1 {
		t.Fatalf("pause suppressed new head transfer: %+v", second)
	}
	pending := map[string]Head{}
	if err := readJSON(filepath.Join(relay.cfg.Store, "pending.json"), &pending, 16<<20); err != nil {
		t.Fatal(err)
	}
	if pending[relayTestID] != newer {
		t.Fatal("pending did not follow new head")
	}
}

func TestPublicationPauseGuardFailsClosed(t *testing.T) {
	n := &nativeAdapter{}
	if reason := n.PublicationPauseReason(context.Background()); reason == "" {
		t.Fatal("missing process guard allowed publication")
	}
	n.processGuard = func(context.Context) (bool, error) { return true, nil }
	if reason := n.PublicationPauseReason(context.Background()); reason == "" {
		t.Fatal("running native processes allowed publication")
	}
	n.processGuard = func(context.Context) (bool, error) { return false, nil }
	if reason := n.PublicationPauseReason(context.Background()); reason != "" {
		t.Fatal("clear runtime did not resume", reason)
	}
}
