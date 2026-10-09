package historysync

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
)

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
