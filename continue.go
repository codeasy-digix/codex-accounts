//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var threadIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type interruptedThread struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Cwd      string `json:"cwd"`
	Reason   string `json:"reason"`
	Stopped  string `json:"stoppedAt"`
	Rollout  string `json:"-"`
	Modified int64  `json:"-"`
	Active   bool   `json:"active"`
	Session  string `json:"tmuxSession,omitempty"`
}

type rolloutIndexEntry struct {
	Size     int64             `json:"size"`
	Modified int64             `json:"modified"`
	Thread   interruptedThread `json:"thread"`
	Blocked  bool              `json:"blocked"`
}

type rolloutRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func limitReason(info json.RawMessage, message string) string {
	var kind string
	json.Unmarshal(info, &kind)
	kind = strings.ToLower(strings.ReplaceAll(kind, "_", ""))
	switch kind {
	case "usagelimitexceeded", "usagelimitreached":
		return "usage limit"
	case "ratelimitexceeded", "ratelimitreached":
		return "rate limit"
	}
	if kind != "" && kind != "other" {
		return ""
	}
	// Old rollouts have no structured error code. Inspect error messages only,
	// never user prompts, assistant text, tool output, or 100% usage snapshots.
	m := strings.ToLower(message)
	for _, phrase := range []string{"you've hit your usage limit", "you have hit your usage limit", "usage limit reached", "usage limit exceeded", "usage_limit_reached", "quota exceeded", "사용 한도에 도달", "사용량 한도에 도달"} {
		if strings.Contains(m, phrase) {
			return "usage limit"
		}
	}
	for _, phrase := range []string{"rate limit reached", "rate limit exceeded", "rate_limit_exceeded", "rate_limit_reached"} {
		if strings.Contains(m, phrase) {
			return "rate limit"
		}
	}
	return ""
}

func inspectRollout(p string) (interruptedThread, bool, error) {
	var thread interruptedThread
	f, err := os.Open(p)
	if err != nil {
		return thread, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16*1024*1024)
	blocked, turnLimited := false, false
	for scanner.Scan() {
		line := scanner.Bytes()
		// Avoid decoding large tool payloads once the display title is known.
		head := line
		if len(head) > 512 {
			head = head[:512]
		}
		if thread.Title != "" && !bytes.Contains(head, []byte(`"event_msg"`)) && !bytes.Contains(head, []byte(`"session_meta"`)) {
			continue
		}
		var record rolloutRecord
		if json.Unmarshal(line, &record) != nil {
			continue // An incomplete final line is normal while a writer is running.
		}
		switch record.Type {
		case "session_meta":
			var meta struct{ ID, Cwd string }
			if json.Unmarshal(record.Payload, &meta) == nil {
				thread.ID, thread.Cwd = meta.ID, meta.Cwd
			}
		case "response_item":
			if thread.Title != "" {
				continue
			}
			var item struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(record.Payload, &item) == nil && item.Role == "user" {
				for _, part := range item.Content {
					if usableTitle(part.Text) {
						thread.Title = displayTitle(part.Text)
						break
					}
				}
			}
		case "event_msg":
			var event struct {
				Type    string          `json:"type"`
				Message string          `json:"message"`
				Info    json.RawMessage `json:"codex_error_info"`
				Error   *struct {
					Message string          `json:"message"`
					Info    json.RawMessage `json:"codex_error_info"`
				} `json:"error"`
			}
			if json.Unmarshal(record.Payload, &event) != nil {
				continue
			}
			switch event.Type {
			case "user_message":
				if thread.Title == "" && usableTitle(event.Message) {
					thread.Title = displayTitle(event.Message)
				}
			case "task_started", "turn_started":
				turnLimited = false
			case "error":
				if reason := limitReason(event.Info, event.Message); reason != "" {
					blocked, turnLimited = true, true
					thread.Reason, thread.Stopped = reason, record.Timestamp
				} else {
					blocked, turnLimited = false, false
				}
			case "task_complete", "turn_complete", "turn_aborted":
				if event.Error != nil {
					reason := limitReason(event.Error.Info, event.Error.Message)
					blocked = reason != ""
					if blocked {
						thread.Reason, thread.Stopped = reason, record.Timestamp
					}
				} else {
					// Legacy task_complete can follow an error without repeating it.
					blocked = turnLimited
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return thread, false, err
	}
	if !threadIDPattern.MatchString(thread.ID) || !filepath.IsAbs(thread.Cwd) {
		return thread, false, nil
	}
	if thread.Title == "" {
		thread.Title = thread.ID
	}
	thread.Rollout = p
	return thread, blocked, nil
}

func usableTitle(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && !strings.HasPrefix(s, "<environment_context>") && !strings.HasPrefix(s, "# AGENTS.md") && !strings.HasPrefix(s, "<INSTRUCTIONS>")
}

func displayTitle(s string) string {
	s = strings.Join(strings.Fields(cleanText(s)), " ")
	runes := []rune(s)
	if len(runes) > 100 {
		s = string(runes[:100]) + "…"
	}
	return s
}

func scanInterrupted(ctxDone <-chan struct{}, shared, accounts string) ([]interruptedThread, int, error) {
	if err := realDirectory(accounts, true); err != nil {
		return nil, 0, err
	}
	indexPath := filepath.Join(accounts, ".continue-index.json")
	index := make(map[string]rolloutIndexEntry)
	if data, _, err := readPrivateFile(indexPath, true); err == nil {
		json.Unmarshal(data, &index)
	}
	if index == nil {
		index = make(map[string]rolloutIndexEntry)
	}
	next := make(map[string]rolloutIndexEntry)
	latest := make(map[string]rolloutIndexEntry)
	skipped := 0
	err := filepath.WalkDir(filepath.Join(shared, "sessions"), func(p string, d fs.DirEntry, walkErr error) error {
		select {
		case <-ctxDone:
			return errors.New("conversation scan cancelled")
		default:
		}
		if walkErr != nil {
			if os.IsNotExist(walkErr) && p == filepath.Join(shared, "sessions") {
				return nil
			}
			skipped++
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			skipped++
			return nil
		}
		entry, cached := index[p]
		if !cached || entry.Size != info.Size() || entry.Modified != info.ModTime().UnixNano() {
			thread, blocked, err := inspectRollout(p)
			if err != nil {
				skipped++
				return nil
			}
			entry = rolloutIndexEntry{info.Size(), info.ModTime().UnixNano(), thread, blocked}
		}
		entry.Thread.Rollout, entry.Thread.Modified = p, entry.Modified
		next[p] = entry
		if previous, ok := latest[entry.Thread.ID]; !ok || previous.Modified < entry.Modified {
			latest[entry.Thread.ID] = entry
		}
		return nil
	})
	if err != nil {
		return nil, skipped, err
	}
	if data, err := json.Marshal(next); err == nil {
		atomicPrivateWrite(indexPath, data) // A cache write failure never hides conversations.
	}
	threads := make([]interruptedThread, 0)
	for _, entry := range latest {
		if !entry.Blocked || !threadIDPattern.MatchString(entry.Thread.ID) {
			continue
		}
		thread := entry.Thread
		thread.Active = threadHasWriter(shared, thread.ID)
		if job, ok := activeContinueJob(accounts, thread.ID); ok {
			thread.Active, thread.Session = true, job.Session
		}
		threads = append(threads, thread)
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].Stopped > threads[j].Stopped })
	return threads, skipped, nil
}

func threadHasWriter(shared, id string) bool {
	fd, err := syscall.Open(filepath.Join(shared, "thread-writer-locks", id+".lock"), syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true // Fail closed when writer ownership cannot be checked.
	}
	defer syscall.Close(fd)
	return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

func (a application) continueAccounts(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: codex continue [--list|--json|--all|UUID]")
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(a.out, "codex continue: list quota-interrupted conversations; select a number, comma-separated numbers, or all.\nUse --list or --json without starting work; --all or UUID launches tmux jobs.\nJobs use this terminal's account and each conversation's original working directory.\nCompleted tmux sessions close automatically; output and status remain in ~/.codex-accounts/.continue-jobs/.")
		return nil
	}
	shared, accounts, err := locations()
	if err != nil {
		return err
	}
	threads, skipped, err := scanInterrupted(a.ctx.Done(), shared, accounts)
	if err != nil {
		return err
	}
	// Show only conversations that can be selected now. Keep the writer check
	// outside the persistent index: a cached quota error can still be active.
	ready := make([]interruptedThread, 0, len(threads))
	activeSkipped := 0
	for _, thread := range threads {
		if thread.Active {
			activeSkipped++
			continue
		}
		ready = append(ready, thread)
	}
	threads = ready
	if len(args) == 1 && args[0] == "--json" {
		return json.NewEncoder(a.out).Encode(map[string]any{"conversations": threads, "skippedFiles": skipped, "activeSkipped": activeSkipped})
	}
	if activeSkipped > 0 {
		fmt.Fprintf(a.err, "Excluded %d active conversation(s) from the stopped-work list.\n", activeSkipped)
	}
	if skipped > 0 {
		fmt.Fprintf(a.err, "Skipped %d unreadable rollout files.\n", skipped)
	}
	if len(threads) == 0 {
		fmt.Fprintln(a.out, "No quota-interrupted conversations found.")
		return nil
	}
	for i, thread := range threads {
		fmt.Fprintf(a.out, "%d. %s\n   %s | %s | %s\n   %s\n", i+1, thread.Title, thread.ID, thread.Reason, thread.Stopped, cleanText(thread.Cwd))
	}
	if len(args) == 1 && args[0] == "--list" {
		return nil
	}
	selection := ""
	if len(args) == 1 {
		selection = args[0]
	} else {
		fmt.Fprint(a.out, "Continue [number(s), all, q]: ")
		selection, err = bufio.NewReader(a.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	selected, err := chooseThreads(threads, strings.TrimSpace(selection))
	if err != nil || len(selected) == 0 {
		return err
	}
	return a.launchContinuations(shared, accounts, selected)
}

func chooseThreads(threads []interruptedThread, selection string) ([]interruptedThread, error) {
	if selection == "" || selection == "q" || selection == "quit" {
		return nil, nil
	}
	if selection == "all" || selection == "a" || selection == "--all" {
		return threads, nil
	}
	selected := make([]interruptedThread, 0)
	seen := make(map[string]bool)
	for _, part := range strings.Split(selection, ",") {
		part = strings.TrimSpace(part)
		var found *interruptedThread
		if n, err := strconv.Atoi(part); err == nil && n > 0 && n <= len(threads) {
			found = &threads[n-1]
		} else {
			for i := range threads {
				if threads[i].ID == part {
					found = &threads[i]
					break
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("unknown conversation selection: %s", cleanText(part))
		}
		if !seen[found.ID] {
			selected = append(selected, *found)
			seen[found.ID] = true
		}
	}
	return selected, nil
}
