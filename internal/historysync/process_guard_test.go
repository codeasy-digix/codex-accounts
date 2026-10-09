package historysync

import "testing"

func TestNativeProcessGuardScopesCurrentUserAndIgnoresZombies(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       bool
	}{
		{"own active CLI", "123 501 R+ codex /usr/local/bin/codex app-server", true},
		{"own sleeping GUI", "124 501 Ss ChatGPT /Applications/ChatGPT.app/Contents/MacOS/ChatGPT", true},
		{"foreign GUI", "125 502 S ChatGPT /Applications/ChatGPT.app/Contents/MacOS/ChatGPT", false},
		{"own zombie", "126 501 Z codex [codex] <defunct>", false},
		{"own zombie modifiers", "127 501 Z+ codex codex <defunct>", false},
		{"self", "999 501 R codex /usr/local/bin/codex", false},
		{"own relay", "128 501 S codex-history-s /usr/local/bin/codex-history-sync serve", false},
		{"ordinary own process", "129 501 SNs node node /tmp/ordinary.js", false},
		{"kernel PID zero", "0 0 S kernel_task kernel_task", false},
		{"foreign plus own", "125 502 S ChatGPT ChatGPT\n123 501 R codex codex", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nativeScopedProcessListingRunning([]byte(tc.line+"\n"), 999, 501)
			if err != nil || got != tc.want {
				t.Fatalf("running=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestNativeProcessGuardMalformedListingsFailClosed(t *testing.T) {
	for _, line := range []string{
		"", "123 501", "bad 501 R codex codex", "-1 501 R codex codex",
		"123 bad R codex codex", "123 -1 R codex codex", "123 +501 R codex codex",
		"123 501 bad codex codex", "123 501 R? codex codex",
		// Even excluded users/zombies must have valid listing metadata.
		"123 502 unknown ChatGPT ChatGPT", "123 bad Z codex codex",
		"123 501 R codex codex\n124 invalid S node node",
	} {
		if _, err := nativeScopedProcessListingRunning([]byte(line+"\n"), 999, 501); err == nil {
			t.Errorf("malformed listing accepted: %q", line)
		}
	}
}
