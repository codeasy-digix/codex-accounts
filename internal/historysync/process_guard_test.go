package historysync

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestNativeProcessGuardExcludesOnlyBundledChromeHost(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".codex", "plugins", "cache", "openai-bundled", "chrome")
	latest := filepath.Join(root, "latest", "extension-host", "macos", "arm64", "ChatGPT for Chrome")
	versioned := filepath.Join(root, "26.1002.52244", "extension-host", "macos", "arm64", "ChatGPT for Chrome")
	otherHome := filepath.Join(t.TempDir(), "other", ".codex", "plugins", "cache", "openai-bundled", "chrome", "latest", "extension-host", "macos", "arm64", "ChatGPT for Chrome")
	unrecognizedVersion := filepath.Join(root, "unverified", "extension-host", "macos", "arm64", "ChatGPT for Chrome")
	for _, tc := range []struct {
		name, line string
		want       bool
	}{
		{"observed spaced macOS host", "75736 501 S " + latest + " " + latest + " chrome-extension://test-host/", false},
		{"resolved numeric version", "75736 501 S " + versioned + " " + versioned, false},
		{"foreign host remains scoped", "75736 502 S " + latest + " " + latest, false},
		{"own zombie remains scoped", "75736 501 Z /Applications/ChatGPT.app/Contents/MacOS/ChatGPT ChatGPT <defunct>", false},
		{"CLI argv cannot spoof host", "123 501 S codex /usr/local/bin/codex app-server --browser-host " + latest, true},
		{"ChatGPT app argv cannot spoof host", "124 501 S /Applications/ChatGPT.app/Contents/MacOS/ChatGPT ChatGPT --browser-host " + latest, true},
		{"Codex app argv cannot spoof host", "125 501 S /Applications/Codex.app/Contents/MacOS/Codex Codex --browser-host " + latest, true},
		{"ChatGPTService remains native", "126 501 S ChatGPTService /Applications/ChatGPT.app/Contents/Frameworks/ChatGPT Service --browser-host " + latest, true},
		{"same name outside bundled path", "127 501 S /tmp/ChatGPT for Chrome /tmp/ChatGPT for Chrome", true},
		{"other home is not excluded", "127 501 S " + otherHome + " ChatGPT", true},
		{"unrecognized version is not excluded", "127 501 S " + unrecognizedVersion + " ChatGPT", true},
		{"host plus actual native writer", "75736 501 S " + latest + " " + latest + "\n123 501 S codex codex app-server", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nativeScopedProcessListingRunning([]byte(tc.line+"\n"), 999, 501)
			if err != nil || got != tc.want {
				t.Fatalf("running=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}
