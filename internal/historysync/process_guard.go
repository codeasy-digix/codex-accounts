package historysync

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The bundled browser plugin installs this OpenAI native messaging executable,
// named "ChatGPT for Chrome", outside either native app. On macOS ps includes
// its spaced executable name in comm. Match only that leading command path;
// a native writer mentioning the browser host in its arguments must still block.
func nativeChromeExtensionHostCommand(fields []string) bool {
	if len(fields) < 4 {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return false
	}
	root := filepath.Join(home, ".codex", "plugins", "cache", "openai-bundled", "chrome") + string(filepath.Separator)
	command := strings.Join(fields[1:4], " ")
	if !strings.HasPrefix(command, root) {
		return false
	}
	relative := strings.TrimPrefix(command, root)
	version, executable, found := strings.Cut(relative, "/")
	if !found || executable != "extension-host/macos/arm64/ChatGPT for Chrome" {
		return false
	}
	if version == "latest" {
		return true
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.TrimLeft(part, "0123456789") != "" {
			return false
		}
	}
	return true
}

// Native stores and their writers are scoped to the current user. Validate the
// complete ps listing before excluding other users and zombie processes, which
// cannot write history, then reuse the existing native command classifier.
func nativeScopedProcessListingRunning(out []byte, selfPID, currentUID int) (bool, error) {
	if currentUID < 0 {
		return false, errors.New("native process guard has an invalid current UID")
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var scoped bytes.Buffer
	count := 0
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return false, errors.New("native process listing is incomplete")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid < 0 || strings.TrimLeft(fields[0], "0123456789") != "" {
			return false, errors.New("native process listing has an invalid PID")
		}
		uid, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil || strings.TrimLeft(fields[1], "0123456789") != "" {
			return false, errors.New("native process listing has an invalid UID")
		}
		state := fields[2]
		// Run states and modifiers emitted by macOS and Linux ps. Unknown states
		// fail closed instead of silently hiding a potentially active writer.
		if !strings.ContainsRune("DIRSTUWXYZtx", rune(state[0])) {
			return false, errors.New("native process listing has an invalid state")
		}
		for _, modifier := range state[1:] {
			if !strings.ContainsRune("<>NLls+AESVWX", modifier) {
				return false, errors.New("native process listing has an invalid state")
			}
		}
		count++
		if uid != uint64(currentUID) || state[0] == 'Z' {
			continue
		}
		scoped.WriteString(fields[0])
		scoped.WriteByte(' ')
		scoped.WriteString(strings.Join(fields[3:], " "))
		scoped.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	if count == 0 {
		return false, errors.New("native process listing is empty")
	}
	return nativeProcessListingRunning(scoped.Bytes(), selfPID)
}
