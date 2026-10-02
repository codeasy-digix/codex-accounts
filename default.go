//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Only the shared home's auth.json may be a managed link. Named credential
// files remain real files, so default and nickname never rotate token copies.
func defaultAccount(shared, accounts string) (string, error) {
	p := filepath.Join(shared, "auth.json")
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode().IsRegular() {
		return "", nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("default auth.json must be a regular file or a managed account link")
	}
	target, err := os.Readlink(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(shared, target)
	}
	target = filepath.Clean(target)
	name := filepath.Base(filepath.Dir(target))
	if !nicknamePattern.MatchString(name) || name == "default" || target != filepath.Join(accounts, name, "auth.json") {
		return "", errors.New("default auth.json points outside the managed accounts; it was not replaced")
	}
	return name, nil
}

func readPrivateFile(p string, optional bool) ([]byte, bool, error) {
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if optional && os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cannot safely read %s", p)
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return nil, false, fmt.Errorf("a regular file of at most 8 MiB is required: %s", p)
	}
	data := make([]byte, info.Size())
	_, err = f.ReadAt(data, 0)
	if len(data) == 0 {
		err = nil
	}
	return data, true, err
}

func atomicPrivateWrite(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".codex-accounts-write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func (a application) makeDefault(shared, accounts, name string, shell bool) error {
	home, err := prepareHome(shared, accounts, name)
	if err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(shared, ".account-default.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), "default lock")
	defer lock.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another terminal is changing the default account; try again when it finishes")
	}
	var backup string
	info, err := a.selectAccountThen(home, shared, false, func(_ statusInfo) error {
		var err error
		backup, err = a.publishDefault(shared, accounts, home)
		return err
	})
	if err != nil {
		return err
	}
	w := a.out
	if shell {
		w = a.err
	}
	fmt.Fprintln(w, "Machine default account:", name)
	if backup != "" {
		fmt.Fprintln(w, "Previous default/config backup:", backup)
	}
	fmt.Fprintln(w, "Restart the GUI app to use this login. Existing terminals keep their selected nickname.")
	printStatus(w, "", shared, shared, info)
	if shell {
		fmt.Fprintln(a.out, "unset CODEX_ACCOUNT CODEX_HOME CODEX_SQLITE_HOME")
	}
	return nil
}

func (a application) publishDefault(shared, accounts, home string) (string, error) {
	previousName, err := defaultAccount(shared, accounts)
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(shared, "config.toml")
	config, configExists, err := readPrivateFile(configPath, true)
	if err != nil {
		return "", err
	}
	var previousAuth []byte
	var authExists bool
	if previousName == "" {
		previousAuth, authExists, err = readPrivateFile(filepath.Join(shared, "auth.json"), true)
		if err != nil {
			return "", err
		}
	}
	backups := filepath.Join(accounts, ".default-backups")
	if err := realDirectory(backups, true); err != nil {
		return "", err
	}
	backup, err := os.MkdirTemp(backups, time.Now().Format("20060102-150405")+"-")
	if err != nil {
		return "", err
	}
	if authExists {
		if err := os.WriteFile(filepath.Join(backup, "auth.json"), previousAuth, 0600); err != nil {
			return "", err
		}
	} else if previousName != "" {
		if err := os.WriteFile(filepath.Join(backup, "previous-account.txt"), []byte(previousName+"\n"), 0600); err != nil {
			return "", err
		}
	}
	if configExists {
		if err := os.WriteFile(filepath.Join(backup, "config.toml"), config, 0600); err != nil {
			return "", err
		}
	}
	binary, err := runtimeBinary()
	if err != nil {
		return "", err
	}
	s, err := newAppServer(a.ctx, binary, shared, shared, false)
	if err != nil {
		return "", err
	}
	defer s.close()
	// Let native Codex edit TOML, preserving other settings and respecting policy.
	var result struct {
		Status string `json:"status"`
	}
	if err := s.request(a.ctx, "config/value/write", map[string]any{
		"keyPath": "cli_auth_credentials_store", "value": "file", "mergeStrategy": "replace", "filePath": configPath,
	}, &result); err != nil {
		return "", errors.New("could not enable native file credentials; the default login was not changed")
	}
	if result.Status != "ok" {
		return "", errors.New("native authentication policy overrides file credentials; the default login was not changed")
	}
	updatedConfig, _, err := readPrivateFile(configPath, false)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(shared, ".default-auth-")
	if err == nil {
		tmp.Close()
		os.Remove(tmp.Name())
		err = os.Symlink(filepath.Join(home, "auth.json"), tmp.Name())
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(shared, "auth.json"))
		}
		os.Remove(tmp.Name())
	}
	if err != nil {
		// Never roll back over a simultaneous user edit.
		current, _, readErr := readPrivateFile(configPath, false)
		if readErr == nil && bytes.Equal(current, updatedConfig) {
			if configExists {
				atomicPrivateWrite(configPath, config)
			} else {
				os.Remove(configPath)
			}
		}
		return "", fmt.Errorf("could not switch the default login; backup: %s", backup)
	}
	return backup, nil
}
