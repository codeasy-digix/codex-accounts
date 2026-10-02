//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginMethodChoiceAndExplicitFlags(t *testing.T) {
	cases := []struct {
		name, input    string
		args           []string
		device, cancel bool
	}{
		{"choose-browser", "2\n", nil, false, false},
		{"choose-device", "1\n", nil, true, false},
		{"default-device", "", nil, true, false},
		{"explicit-browser", "q\n", []string{"--browser"}, false, false},
		{"explicit-device", "q\n", []string{"--device"}, true, false},
		{"cancel", "q\n", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := sandbox(t)
			a, out, stderr := testApp()
			a.in = strings.NewReader(tc.input)
			args := append([]string{"--shell", "fixture"}, tc.args...)
			err := a.account(args)
			calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			if tc.cancel {
				if err == nil || len(calls) != 0 || out.Len() != 0 {
					t.Fatal("cancel started login or emitted environment changes", err)
				}
				return
			}
			if err != nil || !strings.Contains(out.String(), "export CODEX_ACCOUNT='fixture'") || strings.Contains(out.String(), "Login required") {
				t.Fatal("login failed or prompt polluted shell environment", err, out.String(), stderr.String())
			}
			found := false
			for _, line := range strings.Split(string(calls), "\n") {
				var call struct {
					Args []string `json:"args"`
				}
				json.Unmarshal([]byte(line), &call)
				if len(call.Args) > 0 && call.Args[0] == "login" {
					found = true
					device := false
					for _, arg := range call.Args {
						if arg == "--device-auth" {
							device = true
						}
					}
					if device != tc.device {
						t.Fatalf("wrong login command: %v", call.Args)
					}
				}
			}
			if !found {
				t.Fatal("login command was not invoked")
			}
		})
	}
}

func TestValidLoginDoesNotPromptAndConflictingMethodsDoNotLogin(t *testing.T) {
	root, shared, accounts := sandbox(t)
	seed(t, shared, accounts, "fixture")
	a, out, stderr := testApp()
	a.in = strings.NewReader("q\n")
	if err := a.account([]string{"--shell", "fixture"}); err != nil || strings.Contains(stderr.String(), "Choose authentication") {
		t.Fatal("valid login asked to sign in again", err)
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if strings.Contains(string(calls), `"login"`) {
		t.Fatal("valid login reauthenticated")
	}
	a, out, _ = testApp()
	if err := a.account([]string{"--shell", "fixture", "--browser", "--device"}); err == nil || out.Len() != 0 {
		t.Fatal("conflicting methods changed the shell")
	}
}
