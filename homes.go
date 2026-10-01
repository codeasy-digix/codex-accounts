//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
)

var nicknamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var sharedDirectories = []string{"sessions", "archived_sessions", "memories", "attachments", "skills", "rules", "thread-writer-locks", "project-metadata-locks", "rollout-migrations"}
var sharedFiles = []string{"history.jsonl", "session_index.jsonl", "config.toml", "AGENTS.md"}

func locations() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	shared := os.Getenv("CODEX_SHARED_HOME")
	if shared == "" {
		shared = filepath.Join(home, ".codex")
	}
	accounts := os.Getenv("CODEX_ACCOUNTS_DIR")
	if accounts == "" {
		accounts = filepath.Join(home, ".codex-accounts")
	}
	shared, err = filepath.Abs(shared)
	if err != nil {
		return "", "", err
	}
	accounts, err = filepath.Abs(accounts)
	if err != nil {
		return "", "", err
	}
	return shared, accounts, nil
}

func realDirectory(p string, private bool) error {
	info, err := os.Lstat(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("a real directory is required: %s", p)
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	if private {
		return os.Chmod(p, 0700)
	}
	return nil
}

func inside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) >= 1 && !(len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator))
}

func prepareHome(shared, accounts, name string) (string, error) {
	if !nicknamePattern.MatchString(name) || name == "default" {
		return "", errors.New("account name must start with a lowercase letter/digit, use a-z, 0-9, _ or -, and be at most 64 characters; default is reserved")
	}
	if inside(accounts, shared) || inside(shared, accounts) {
		return "", errors.New("account credentials and the shared store must be separate directories")
	}
	if err := realDirectory(shared, false); err != nil {
		return "", err
	}
	if err := realDirectory(accounts, true); err != nil {
		return "", err
	}
	resolvedShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		return "", err
	}
	resolvedAccounts, err := filepath.EvalSymlinks(accounts)
	if err != nil {
		return "", err
	}
	if inside(resolvedAccounts, resolvedShared) || inside(resolvedShared, resolvedAccounts) {
		return "", errors.New("account directory resolves inside or around the shared store")
	}
	// Keep the user's absolute path for compatibility with existing CODEX_HOME values.
	home := filepath.Join(accounts, name)
	if err := realDirectory(home, true); err != nil {
		return "", err
	}
	if err := privateAuth(home); err != nil {
		return "", err
	}
	names := append(append([]string{}, sharedDirectories...), sharedFiles...)
	// Preflight all destinations before creating links; never replace user files.
	for _, name := range names {
		dest := filepath.Join(home, name)
		info, err := os.Lstat(dest)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("existing data was not overwritten: %s", dest)
		}
		target, err := os.Readlink(dest)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(home, target)
		}
		if filepath.Clean(target) != filepath.Join(shared, name) && filepath.Clean(target) != filepath.Join(resolvedShared, name) {
			return "", fmt.Errorf("existing link points elsewhere: %s", dest)
		}
	}
	for _, name := range sharedDirectories {
		if err := os.MkdirAll(filepath.Join(shared, name), 0700); err != nil {
			return "", err
		}
	}
	for _, name := range []string{"history.jsonl", "session_index.jsonl", "config.toml"} {
		f, err := os.OpenFile(filepath.Join(shared, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil && !os.IsExist(err) {
			return "", err
		}
		if f != nil {
			f.Close()
		}
	}
	for _, name := range names {
		dest := filepath.Join(home, name)
		if _, err := os.Lstat(dest); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(shared, name), dest); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	return home, nil
}

func privateAuth(home string) error {
	p := filepath.Join(home, "auth.json")
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("auth.json must be a real, account-specific file")
	}
	return os.Chmod(p, 0600)
}

func loginLock(home string) (*os.File, error) {
	fd, err := syscall.Open(filepath.Join(home, ".account-login.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "account-login.lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another terminal is checking or logging in to this account; try again when it finishes")
	}
	return f, nil
}

func listAccounts(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && nicknamePattern.MatchString(entry.Name()) && entry.Name() != "default" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func setEnvironment(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, item := range env {
		if len(item) > len(key) && item[:len(key)+1] == key+"=" {
			continue
		}
		result = append(result, item)
	}
	return append(result, key+"="+value)
}

func accountEnvironment(env []string, home, shared string, isolated bool) []string {
	result := make([]string, 0, len(env)+2)
	for _, item := range env {
		key, _, _ := cutEnvironment(item)
		if isolated && (key == "OPENAI_API_KEY" || key == "CODEX_API_KEY" || key == "CODEX_ACCESS_TOKEN") {
			continue
		}
		result = append(result, item)
	}
	result = setEnvironment(result, "CODEX_HOME", home)
	return setEnvironment(result, "CODEX_SQLITE_HOME", shared)
}

func cutEnvironment(item string) (string, string, bool) {
	for i := range item {
		if item[i] == '=' {
			return item[:i], item[i+1:], true
		}
	}
	return item, "", false
}
