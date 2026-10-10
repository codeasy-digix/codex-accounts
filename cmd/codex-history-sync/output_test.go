package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeasy-digix/codex-accounts/internal/historysync"
)

func TestDefaultStatusIsBoundedAndExplicitJSONPreservesReport(t *testing.T) {
	root := t.TempDir()
	cfg := historysync.Config{Node: "test", Home: filepath.Join(root, "native-absent"), Store: filepath.Join(root, "store"), HubStore: filepath.Join(root, "hub"), Enabled: true}
	if err := os.MkdirAll(cfg.Store, 0700); err != nil {
		t.Fatal(err)
	}
	report := historysync.CycleReport{Node: cfg.Node, Finished: time.Now(), Deferred: 1000}
	for i := 0; i < 1000; i++ {
		report.Results = append(report.Results, historysync.InstallResult{ID: fmt.Sprintf("PRIVATE_ID_%d", i), Digest: "PRIVATE_DIGEST", Status: "deferred", Reason: "native Codex processes are running; publication deferred"})
	}
	for i := 0; i < 20; i++ {
		report.Errors = append(report.Errors, strings.Repeat("error text ", 100))
	}
	for path, value := range map[string]any{filepath.Join(root, "config.json"): cfg, filepath.Join(cfg.Store, "last-report.json"): report} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config", filepath.Join(root, "config.json"), "status"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "PRIVATE_ID_") || strings.Contains(out.String(), "PRIVATE_DIGEST") || strings.Count(out.String(), "\n") > 12 || len(out.Bytes()) > 2000 || !strings.Contains(out.String(), "1000건") || !strings.Contains(out.String(), "오류: 20건") {
		t.Fatal("default output is unbounded or hides failures", out.String())
	}
	for _, flag := range []string{"--json", "--verbose"} {
		out.Reset()
		if err := run(context.Background(), []string{flag, "status", "--config", filepath.Join(root, "config.json")}, nil, &out); err != nil {
			t.Fatal(err)
		}
		var status statusReport
		if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Report == nil || len(status.Report.Results) != 1000 || len(status.Report.Errors) != 20 {
			t.Fatal("explicit complete report lost data", flag, err)
		}
	}
	if _, err := os.Stat(cfg.Home); !os.IsNotExist(err) {
		t.Fatal("status touched native conversations", err)
	}
}

func TestConflictsPrioritizesUnresolvedAndOmitsRevisions(t *testing.T) {
	state := historysync.HubState{ConflictsTotal: 15, RevisionsTotal: 1}
	for i := 0; i < 15; i++ {
		state.Conflicts = append(state.Conflicts, historysync.Conflict{ID: fmt.Sprintf("id-%02d", i), Resolved: i != 0, Observed: time.Unix(int64(i), 0), Reason: "different history"})
	}
	state.Conflicts = append(state.Conflicts, historysync.Conflict{ID: "NORMAL_REVISION", Revision: true})
	var out bytes.Buffer
	if err := printConflicts(&out, state); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "미해결 1") || !strings.Contains(lines[1], "id-00 [미해결]") || !strings.Contains(out.String(), "외 5건") || strings.Contains(out.String(), "NORMAL_REVISION") || len(lines) != 13 {
		t.Fatal("conflicts summary is not bounded or prioritizes the wrong records", out.String())
	}
}

func TestUnavailableHubDoesNotImplyZeroCurrentConflicts(t *testing.T) {
	var out bytes.Buffer
	err := printStatus(&out, statusReport{Node: "test", Report: &historysync.CycleReport{ConflictsTotal: 7}, HubError: strings.Repeat("offline ", 100)})
	if err != nil || !strings.Contains(out.String(), "이전 기록; 현재 상태 확인 불가") || strings.Contains(out.String(), "미해결 0") {
		t.Fatal("failed hub query hid the historical conflict count", err, out.String())
	}
}
