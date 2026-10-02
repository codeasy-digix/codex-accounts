//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromoteDefaultSharesOneCredentialAndBacksUp(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	old := []byte(`{"tokens":{"refresh_token":"previous-default-fixture"}}`)
	config := []byte("# retain this setting\nmodel = \"fixture-model\"\n")
	os.WriteFile(filepath.Join(shared, "auth.json"), old, 0600)
	os.WriteFile(filepath.Join(shared, "config.toml"), config, 0600)
	os.WriteFile(filepath.Join(shared, "history.jsonl"), []byte("retain history\n"), 0600)
	a, out, stderr := testApp()
	if err := a.account([]string{"--shell", "ryu", "default"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "unset CODEX_ACCOUNT CODEX_HOME CODEX_SQLITE_HOME" {
		t.Fatal("default promotion polluted shell output")
	}
	if !strings.Contains(stderr.String(), "Machine default account: ryu") || !strings.Contains(stderr.String(), "Email: ryu@example.test") {
		t.Fatal(stderr.String())
	}
	if name, err := defaultAccount(shared, accounts); err != nil || name != "ryu" {
		t.Fatal(name, err)
	}
	if info, _ := os.Lstat(shared); !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("shared home role changed")
	}
	if info, _ := os.Lstat(filepath.Join(home, "auth.json")); !info.Mode().IsRegular() {
		t.Fatal("nickname credentials are no longer real")
	}
	backups, _ := os.ReadDir(filepath.Join(accounts, ".default-backups"))
	if len(backups) != 1 {
		t.Fatal("missing default backup")
	}
	backup := filepath.Join(accounts, ".default-backups", backups[0].Name())
	data, _ := os.ReadFile(filepath.Join(backup, "auth.json"))
	if !bytes.Equal(data, old) {
		t.Fatal("old default credentials not retained")
	}
	data, _ = os.ReadFile(filepath.Join(backup, "config.toml"))
	if !bytes.Equal(data, config) {
		t.Fatal("old config not retained")
	}
	for _, p := range []string{backup, filepath.Join(backup, "auth.json"), filepath.Join(backup, "config.toml")} {
		info, _ := os.Stat(p)
		if info.Mode().Perm()&0077 != 0 {
			t.Fatal("backup is not private")
		}
	}
	data, _ = os.ReadFile(filepath.Join(shared, "history.jsonl"))
	if string(data) != "retain history\n" {
		t.Fatal("history changed")
	}
	// Native refresh/save follows this one file, rather than stale token copies.
	updated := []byte(`{"tokens":{"refresh_token":"updated-fixture"}}`)
	os.WriteFile(filepath.Join(shared, "auth.json"), updated, 0600)
	data, _ = os.ReadFile(filepath.Join(home, "auth.json"))
	if !bytes.Equal(data, updated) {
		t.Fatal("default and nickname tokens diverged")
	}
	other := seed(t, shared, accounts, "other")
	a, _, _ = testApp()
	if err := a.account([]string{"other", "default"}); err != nil {
		t.Fatal(err)
	}
	if link, _ := os.Readlink(filepath.Join(shared, "auth.json")); link != filepath.Join(other, "auth.json") {
		t.Fatal("second default did not take effect")
	}
	data, _ = os.ReadFile(filepath.Join(home, "auth.json"))
	if !bytes.Equal(data, updated) {
		t.Fatal("previous nickname credentials changed")
	}
}

func TestDefaultPromotionFailureKeepsDefaultAndShell(t *testing.T) {
	for _, mode := range []string{"network", "policy", "unsafe-link"} {
		t.Run(mode, func(t *testing.T) {
			_, shared, accounts := sandbox(t)
			seed(t, shared, accounts, "ryu")
			old := []byte(`{"tokens":{"refresh_token":"default-fixture"}}`)
			p := filepath.Join(shared, "auth.json")
			if mode == "unsafe-link" {
				os.Symlink(filepath.Join(t.TempDir(), "auth.json"), p)
			} else {
				os.WriteFile(p, old, 0600)
				t.Setenv("CODEX_ACCOUNTS_TEST_MODE", mode)
			}
			a, out, stderr := testApp()
			if err := a.account([]string{"--shell", "ryu", "default"}); err == nil || out.Len() != 0 {
				t.Fatal("failed promotion changed shell")
			}
			if strings.Contains(stderr.String(), "SECRET-FIXTURE") {
				t.Fatal("error body leaked")
			}
			if mode != "unsafe-link" {
				data, _ := os.ReadFile(p)
				if !bytes.Equal(data, old) {
					t.Fatal("failed promotion changed default")
				}
			}
		})
	}
}

func TestRealDefaultCredentialLink(t *testing.T) {
	binary := os.Getenv("CODEX_ACCOUNTS_INTEGRATION_RUNTIME")
	if binary == "" {
		t.Skip("set CODEX_ACCOUNTS_INTEGRATION_RUNTIME for native integration")
	}
	_, shared, accounts := sandbox(t)
	home, err := prepareHome(shared, accounts, "chosen")
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic API-key storage exercises the native save path without a login
	// ceremony, network request, model turn, or real credential on any machine.
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-before"}`), 0600)
	os.WriteFile(filepath.Join(shared, "config.toml"), []byte("# keep this comment\nmodel = \"gpt-6.1-sol\"\ncli_auth_credentials_store = \"file\"\n"), 0600)
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", binary)
	a, _, _ := testApp()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.ctx = ctx
	if _, err := a.publishDefault(shared, accounts, home); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(shared, "config.toml"))
	if !strings.Contains(string(config), "# keep this comment") || !strings.Contains(string(config), `model = "gpt-6.1-sol"`) {
		t.Fatal("native config edit lost unrelated settings")
	}
	cmd := exec.CommandContext(ctx, binary, "login", "--with-api-key", "-c", `cli_auth_credentials_store="file"`)
	cmd.Env = accountEnvironment(os.Environ(), shared, shared, true)
	cmd.Stdin = strings.NewReader("synthetic-after\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native local credential save failed: %v %s", err, output)
	}
	if _, err := os.Readlink(filepath.Join(shared, "auth.json")); err != nil {
		t.Fatal("native save replaced the default link")
	}
	data, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	var auth map[string]any
	if json.Unmarshal(data, &auth) != nil || auth["OPENAI_API_KEY"] != "synthetic-after" {
		t.Fatal("native save did not update the one credential store")
	}
}
