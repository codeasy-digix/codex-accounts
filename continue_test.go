//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func makeRollout(t *testing.T, shared, cwd, id, events string) string {
	t.Helper()
	dir := filepath.Join(shared, "sessions", "2026", "10", "02")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-10-02T00-00-00-"+id+".jsonl")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd}})
	data := string(meta) + "\n" + `{"type":"event_msg","payload":{"type":"user_message","message":"Fix the fixture project"}}` + "\n" + events
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

const quotaEvent = `{"timestamp":"2026-10-02T00:00:00Z","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit","codex_error_info":"UsageLimitExceeded"}}` + "\n"

func TestContinueDetectsOnlyUnresolvedLimitFailures(t *testing.T) {
	cases := []struct {
		name, events string
		blocked      bool
	}{
		{"structured", quotaEvent, true},
		{"legacy-complete", quotaEvent + `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}` + "\n", true},
		{"terminal-error", `{"type":"event_msg","payload":{"type":"task_complete","error":{"codex_error_info":"rate_limit_exceeded"}}}` + "\n", true},
		{"completed-after", quotaEvent + `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}` + "\n", false},
		{"network-error", `{"type":"event_msg","payload":{"type":"error","message":"503 connection failure"}}` + "\n", false},
		{"text-only", `{"type":"event_msg","payload":{"type":"user_message","message":"Explain usage limit exceeded"}}` + "\n", false},
		{"quota-snapshot", `{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":100}}}}` + "\n", false},
		{"other-terminal-error", quotaEvent + `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","error":{"codex_error_info":"Unauthorized"}}}` + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, shared, _ := sandbox(t)
			p := makeRollout(t, shared, root, "f223f119-871b-4e7c-9b6d-214cd8e8ea23", tc.events)
			thread, blocked, err := inspectRollout(p)
			if err != nil || blocked != tc.blocked || thread.Cwd != root || thread.Title != "Fix the fixture project" {
				t.Fatalf("unexpected classification: %v %v %+v", err, blocked, thread)
			}
		})
	}
}

func TestContinueCacheAndActiveWriterProtection(t *testing.T) {
	root, shared, _ := sandbox(t)
	id := "f223f119-871b-4e7c-9b6d-214cd8e8ea23"
	p := makeRollout(t, shared, root, id, quotaEvent)
	a, out, _ := testApp()
	if err := a.continueAccounts([]string{"--json"}); err != nil || !strings.Contains(out.String(), id) {
		t.Fatal(err, out.String())
	}
	os.MkdirAll(filepath.Join(shared, "thread-writer-locks"), 0700)
	f, _ := os.OpenFile(filepath.Join(shared, "thread-writer-locks", id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	a, out, _ = testApp()
	if err := a.continueAccounts([]string{"--json"}); err != nil || !strings.Contains(out.String(), `"active":true`) {
		t.Fatal("active thread was not protected", err, out.String())
	}
	appendix := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	f2, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	f2.WriteString(appendix)
	f2.Close()
	a, out, _ = testApp()
	if err := a.continueAccounts([]string{"--list"}); err != nil || !strings.Contains(out.String(), "No quota-interrupted") {
		t.Fatal("cached limit survived completion", err, out.String())
	}
}

func TestContinueTmuxBatchUsesCallerEnvironmentAndCleansUp(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root, shared, accounts := sandbox(t)
	cwd := filepath.Join(root, "it's a fixture project")
	os.MkdirAll(cwd, 0700)
	home := seed(t, shared, accounts, "ryu")
	t.Setenv("CODEX_ACCOUNT", "ryu")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SQLITE_HOME", shared)
	t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "continue")
	t.Setenv("OPENAI_API_KEY", "synthetic-env-fixture")
	t.Setenv("CODEX_ACCOUNTS_TMUX_SOCKET", fmt.Sprintf("codex-accounts-test-%d", time.Now().UnixNano()))
	t.Cleanup(func() { exec.Command("tmux", tmuxArgs("kill-server")...).Run() })
	// An existing server has another account in its environment. Workers must
	// still use the launching terminal's account and preserve unrelated sessions.
	anchor := exec.Command("tmux", tmuxArgs("new-session", "-d", "-s", "codex-accounts-test-anchor", "sleep 30")...)
	anchor.Env = setEnvironment(os.Environ(), "CODEX_ACCOUNT", "other")
	anchor.Env = setEnvironment(anchor.Env, "CODEX_HOME", filepath.Join(accounts, "other"))
	if output, err := anchor.CombinedOutput(); err != nil {
		t.Fatalf("fixture tmux anchor failed: %v %s", err, output)
	}
	ids := []string{"f223f119-871b-4e7c-9b6d-214cd8e8ea23", "f223f119-871b-4e7c-9b6d-214cd8e8ea24"}
	for _, id := range ids {
		makeRollout(t, shared, cwd, id, quotaEvent)
	}
	cmd := exec.Command(testCLI, "continue", "--all")
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("Started 2 conversation(s)")) || !bytes.Contains(output, []byte("Attach: tmux")) {
		t.Fatalf("batch launch failed: %v %s", err, output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for _, id := range ids {
		var job continueJob
		for {
			data, _ := os.ReadFile(jobPath(accounts, id))
			job = continueJob{}
			json.Unmarshal(data, &job)
			if job.State == "completed" || job.State == "failed" || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if job.State != "completed" || len(job.Env) != 0 || job.Account != "ryu" {
			t.Fatalf("worker failed or retained environment: state=%s account=%s", job.State, job.Account)
		}
		for exec.Command("tmux", tmuxArgs("has-session", "-t", "="+job.Session)...).Run() == nil && time.Now().Before(deadline) {
			time.Sleep(30 * time.Millisecond)
		}
		if _, active := activeContinueJob(accounts, id); active {
			t.Fatal("finished tmux session not cleared")
		}
		log, _ := os.ReadFile(filepath.Join(accounts, ".continue-jobs", id+".log"))
		if !bytes.Contains(log, []byte("fixture continuation completed")) {
			t.Fatal("continuation output not retained")
		}
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if strings.Count(string(calls), `"exec","resume","--skip-git-repo-check"`) != 2 || !bytes.Contains(calls, []byte(`"home":"`+home+`"`)) {
		t.Fatal("workers did not use the caller's account and exact thread ids")
	}
	scanner := bufio.NewScanner(bytes.NewReader(calls))
	for scanner.Scan() {
		var call struct {
			Args []string `json:"args"`
			Cwd  string   `json:"cwd"`
			Home string   `json:"home"`
			API  bool     `json:"apiOverridePresent"`
		}
		json.Unmarshal(scanner.Bytes(), &call)
		if strings.Contains(strings.Join(call.Args, " "), "exec resume") && (call.Cwd != cwd || call.Home != home || call.API) {
			t.Fatal("worker inherited the tmux server's account, API override, or wrong cwd")
		}
	}
	if exec.Command("tmux", tmuxArgs("has-session", "-t", "=codex-accounts-test-anchor")...).Run() != nil {
		t.Fatal("job cleanup removed an unrelated session")
	}
}

func TestContinueSelectionAndInteractiveCancellation(t *testing.T) {
	threads := []interruptedThread{{ID: "one"}, {ID: "two"}}
	selected, err := chooseThreads(threads, "2,1,2")
	if err != nil || len(selected) != 2 || selected[0].ID != "two" {
		t.Fatal("number selection/deduplication failed")
	}
	if _, err := chooseThreads(threads, "1,bad"); err == nil {
		t.Fatal("partially invalid selection accepted")
	}
	root, shared, _ := sandbox(t)
	makeRollout(t, shared, root, "f223f119-871b-4e7c-9b6d-214cd8e8ea23", quotaEvent)
	a, out, _ := testApp()
	a.in = strings.NewReader("q\n")
	if err := a.continueAccounts(nil); err != nil || !strings.Contains(out.String(), "Continue [number(s)") || strings.Contains(out.String(), "Started") {
		t.Fatal("interactive cancel launched work", err)
	}
}
