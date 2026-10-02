//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContinueGroupsFiltersAndCacheMigration(t *testing.T) {
	root, shared, accounts := sandbox(t)
	quotaID := "f223f119-871b-4e7c-9b6d-214cd8e8ea23"
	otherID := "f223f119-871b-4e7c-9b6d-214cd8e8ea24"
	completedID := "f223f119-871b-4e7c-9b6d-214cd8e8ea25"
	makeRollout(t, shared, root, quotaID, quotaEvent)
	otherFile := makeRollout(t, shared, root, otherID, otherEvent)
	makeRollout(t, shared, root, completedID, startedEvent+completedEvent)
	// A pre-upgrade cache would hide non-quota interruptions forever unless the
	// parser schema invalidates it even when file size and mtime are unchanged.
	info, _ := os.Stat(otherFile)
	os.MkdirAll(accounts, 0700)
	old, _ := json.Marshal(map[string]rolloutIndexEntry{otherFile: {Size: info.Size(), Modified: info.ModTime().UnixNano(), Blocked: false}})
	os.WriteFile(filepath.Join(accounts, ".continue-index.json"), old, 0600)
	for _, category := range []string{"", quotaCategory, otherCategory} {
		a, out, _ := testApp()
		args := []string{"--json"}
		if category != "" {
			args = append(args, "--"+category)
		}
		if err := a.continueAccounts(args); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Conversations []interruptedThread `json:"conversations"`
			Counts        map[string]int      `json:"counts"`
		}
		if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		want := 2
		if category != "" {
			want = 1
		}
		if len(reply.Conversations) != want {
			t.Fatalf("wrong %s group: %s", category, out.String())
		}
		for _, thread := range reply.Conversations {
			if thread.ID == completedID || (category != "" && thread.Category != category) {
				t.Fatal("completed or wrong-category work was listed", out.String())
			}
		}
	}
	a, out, _ := testApp()
	if err := a.continueAccounts([]string{"--list"}); err != nil || !strings.Contains(out.String(), "Quota interruptions (1)") || !strings.Contains(out.String(), "Other interruptions (1)") {
		t.Fatal("separate groups were not shown", err, out.String())
	}
	for _, args := range [][]string{{"--quota", "--other"}, {"--all", "--list"}, {"--list", "--json"}, {"--unknown"}} {
		a, out, _ := testApp()
		if err := a.continueAccounts(args); err == nil || out.Len() != 0 {
			t.Fatal("invalid flags were accepted", args)
		}
	}
}

func TestContinueCategoryBatchSelection(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	for _, category := range []string{quotaCategory, otherCategory, "all", "number-quota", "number-other", "number-all", "number-retry"} {
		t.Run(category, func(t *testing.T) {
			root, shared, accounts := sandbox(t)
			t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "continue")
			t.Setenv("CODEX_ACCOUNTS_TMUX_SOCKET", fmt.Sprintf("codex-accounts-groups-%d", time.Now().UnixNano()))
			t.Cleanup(func() { exec.Command("tmux", tmuxArgs("kill-server")...).Run() })
			quotaID := "f223f119-871b-4e7c-9b6d-214cd8e8ea23"
			otherID := "f223f119-871b-4e7c-9b6d-214cd8e8ea24"
			makeRollout(t, shared, root, quotaID, quotaEvent)
			makeRollout(t, shared, root, otherID, otherEvent)
			makeRollout(t, shared, root, "f223f119-871b-4e7c-9b6d-214cd8e8ea25", startedEvent+completedEvent)
			args := []string{"continue", "--all"}
			wanted := map[string]bool{quotaID: true, otherID: true}
			input := ""
			switch category {
			case "number-quota":
				args, input = []string{"continue"}, "3\n"
				delete(wanted, otherID)
			case "number-other":
				args, input = []string{"continue"}, "4\n"
				delete(wanted, quotaID)
			case "number-all":
				args, input = []string{"continue"}, "5\n"
			case "number-retry":
				args, input = []string{"continue"}, "99\n2\n"
				delete(wanted, quotaID)
			}
			if category == quotaCategory || category == otherCategory {
				args = append(args, "--"+category)
				if category == quotaCategory {
					delete(wanted, otherID)
				} else {
					delete(wanted, quotaID)
				}
			}
			cmd := exec.Command(testCLI, args...)
			cmd.Stdin = strings.NewReader(input)
			output, err := cmd.CombinedOutput()
			if err != nil || !bytes.Contains(output, []byte(fmt.Sprintf("Started %d conversation(s)", len(wanted)))) {
				t.Fatalf("wrong batch: %v %s", err, output)
			}
			deadline := time.Now().Add(10 * time.Second)
			for id := range wanted {
				for {
					var job continueJob
					data, _ := os.ReadFile(jobPath(accounts, id))
					json.Unmarshal(data, &job)
					if job.State == "completed" {
						break
					}
					if job.State == "failed" || time.Now().After(deadline) {
						t.Fatalf("worker failed: %+v", job)
					}
					time.Sleep(30 * time.Millisecond)
				}
			}
			calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			resumed := map[string]bool{}
			for _, line := range strings.Split(string(calls), "\n") {
				var call struct {
					Args []string `json:"args"`
				}
				json.Unmarshal([]byte(line), &call)
				if len(call.Args) >= 2 && call.Args[len(call.Args)-1] == "continue" {
					id := call.Args[len(call.Args)-2]
					if !wanted[id] {
						t.Fatal("resumed an unselected category", id)
					}
					resumed[id] = true
				}
			}
			if len(resumed) != len(wanted) {
				t.Fatal("not all selected work resumed", resumed)
			}
		})
	}
}

func TestContinueNumberMenuCancelAndInvalidInputStartsNothing(t *testing.T) {
	for _, input := range []string{"0\n", "99\n0\n", "99", ""} {
		t.Run(strings.ReplaceAll(input, "\n", "-"), func(t *testing.T) {
			root, shared, _ := sandbox(t)
			makeRollout(t, shared, root, "f223f119-871b-4e7c-9b6d-214cd8e8ea23", quotaEvent)
			makeRollout(t, shared, root, "f223f119-871b-4e7c-9b6d-214cd8e8ea24", otherEvent)
			a, out, stderr := testApp()
			a.in = strings.NewReader(input)
			err := a.continueAccounts(nil)
			if (input == "99") != (err != nil) {
				t.Fatal("wrong EOF/cancel result", err)
			}
			for _, label := range []string{"3. Continue all quota", "4. Continue all other", "5. Continue all listed", "0. Cancel", "Select a number"} {
				if !strings.Contains(out.String(), label) {
					t.Fatal("numbered option missing", label, out.String())
				}
			}
			if input == "99\n0\n" && !strings.Contains(stderr.String(), "Invalid selection") {
				t.Fatal("invalid input was not explained")
			}
			if _, err := os.Stat(filepath.Join(root, "calls.jsonl")); !os.IsNotExist(err) {
				t.Fatal("listing/cancellation invoked native Codex")
			}
		})
	}
}

func TestContinueInteractiveCategorySelectionAndChangedState(t *testing.T) {
	threads := []interruptedThread{{ID: "quota", Category: quotaCategory}, {ID: "other", Category: otherCategory}}
	for _, category := range []string{quotaCategory, otherCategory} {
		selected, err := chooseThreads(threads, category)
		if err != nil || len(selected) != 1 || selected[0].Category != category {
			t.Fatal("interactive group selection failed", category)
		}
	}
	current := interruptedThread{ID: "same", Category: quotaCategory, Cwd: "/fixture", Stopped: "now", Reason: "usage limit"}
	changed := current
	changed.Category, changed.Reason = otherCategory, "user interruption"
	if sameInterruptedState(current, changed) {
		t.Fatal("different interruption accepted after selection")
	}
}
