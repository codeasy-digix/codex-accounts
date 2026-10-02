//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var threadIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const (
	quotaCategory         = "quota"
	otherCategory         = "other"
	continueParserVersion = 2
)

type interruptedThread struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Cwd      string `json:"cwd"`
	Reason   string `json:"reason"`
	Category string `json:"category"`
	Stopped  string `json:"stoppedAt"`
	Rollout  string `json:"-"`
	Modified int64  `json:"-"`
	Active   bool   `json:"active"`
	Session  string `json:"tmuxSession,omitempty"`
}

type rolloutIndexEntry struct {
	Parser   int               `json:"parserVersion"`
	Size     int64             `json:"size"`
	Modified int64             `json:"modified"`
	Thread   interruptedThread `json:"thread"`
	Blocked  bool              `json:"blocked"`
}

func errorKind(info json.RawMessage) string {
	var kind string
	if json.Unmarshal(info, &kind) != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(info, &object) == nil {
			if typed, ok := object["type"]; ok {
				json.Unmarshal(typed, &kind)
			} else if len(object) == 1 {
				for key := range object {
					kind = key
				}
			}
		}
	}
	return strings.ToLower(strings.ReplaceAll(kind, "_", ""))
}

type rolloutRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func limitReason(info json.RawMessage, message string) string {
	kind := errorKind(info)
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

func failureReason(info json.RawMessage, message string) (category, reason string, affectsTurn bool) {
	if reason := limitReason(info, message); reason != "" {
		return quotaCategory, reason, true
	}
	// These errors reject a control operation without failing the current turn.
	switch errorKind(info) {
	case "threadrollbackfailed", "activeturnnotsteerable":
		return "", "", false
	case "httpconnectionfailed", "responsestreamconnectionfailed", "responsestreamdisconnected", "responsetoomanyfailedattempts":
		reason = "network/stream error"
	case "unauthorized":
		reason = "authentication error"
	case "serveroverloaded", "internalservererror":
		reason = "server error"
	case "contextwindowexceeded":
		reason = "context window exceeded"
	case "sessionbudgetexceeded":
		reason = "session budget exceeded"
	case "sandboxerror":
		reason = "sandbox error"
	case "badrequest", "invalidprompt":
		reason = "request error"
	case "toomanydenials":
		reason = "approval denied"
	case "flexunavailable":
		reason = "model unavailable"
	case "cyberpolicy", "biopolicy", "misalignmentpolicyviolation":
		reason = "policy error"
	default:
		reason = "execution error"
	}
	// Use stable labels rather than printing arbitrary provider error bodies.
	return otherCategory, reason, true
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
	blocked, turnFailed := false, false
	currentTurn := ""
	mark := func(category, reason, timestamp string) {
		blocked = true
		thread.Category, thread.Reason, thread.Stopped = category, reason, timestamp
	}
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
				Type        string          `json:"type"`
				TurnID      string          `json:"turn_id"`
				Reason      string          `json:"reason"`
				Message     string          `json:"message"`
				LastMessage *string         `json:"last_agent_message"`
				Info        json.RawMessage `json:"codex_error_info"`
				Error       *struct {
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
				currentTurn, turnFailed = event.TurnID, false
				mark(otherCategory, "unfinished turn; no active writer", record.Timestamp)
			case "error":
				if category, reason, affects := failureReason(event.Info, event.Message); affects {
					mark(category, reason, record.Timestamp)
					turnFailed = true
				}
			case "stream_error":
				// Stream errors can be retried within a turn. A later successful
				// completion clears them; an exited writer leaves other stopped work.
				if !turnFailed {
					mark(otherCategory, "network/stream error", record.Timestamp)
				}
			case "task_complete", "turn_complete", "turn_aborted":
				if currentTurn != "" && event.TurnID != "" && currentTurn != event.TurnID {
					continue // A late terminal marker for a replaced, older turn.
				}
				terminalError := false
				if event.Error != nil {
					if category, reason, affects := failureReason(event.Error.Info, event.Error.Message); affects {
						mark(category, reason, record.Timestamp)
						terminalError, turnFailed = true, true
					}
				}
				if terminalError {
					continue
				}
				if event.Type == "turn_aborted" {
					if !turnFailed {
						reason := "turn aborted"
						switch event.Reason {
						case "interrupted":
							reason = "user interruption"
						case "replaced":
							reason = "turn replaced"
						case "review_ended":
							reason = "review ended"
						case "budget_limited":
							reason = "session budget exceeded"
						}
						mark(otherCategory, reason, record.Timestamp)
					}
				} else {
					// Legacy completion may follow a fatal error without repeating it.
					// An actual final response confirms same-turn recovery.
					blocked = turnFailed && (event.LastMessage == nil || strings.TrimSpace(*event.LastMessage) == "")
					if !blocked {
						turnFailed, currentTurn = false, ""
					}
				}
			case "shutdown_complete":
				if blocked && !turnFailed {
					mark(otherCategory, "process shut down before turn completed", record.Timestamp)
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
	if blocked && thread.Stopped == "" {
		if info, err := f.Stat(); err == nil {
			thread.Stopped = info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
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
		if !cached || entry.Parser != continueParserVersion || entry.Size != info.Size() || entry.Modified != info.ModTime().UnixNano() {
			thread, blocked, err := inspectRollout(p)
			if err != nil {
				skipped++
				return nil
			}
			entry = rolloutIndexEntry{Parser: continueParserVersion, Size: info.Size(), Modified: info.ModTime().UnixNano(), Thread: thread, Blocked: blocked}
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
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].Stopped == threads[j].Stopped {
			return threads[i].ID < threads[j].ID
		}
		return threads[i].Stopped > threads[j].Stopped
	})
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
