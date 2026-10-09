#!/usr/bin/env python3
"""Prepare an OS service for an existing history relay; no Python at runtime."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import pwd
import re
import shlex
import shutil
import subprocess

LABEL = 'kr.digix.codex-history-sync'
UNIT = 'codex-history-sync.service'


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def service_definition(home, username, system):
    home = Path(home)
    binary = home / '.local/bin/codex-history-sync'
    config = home / '.codex-history-sync/v2/config.json'
    path = ':'.join(map(str, [home / '.local/bin', home / 'homebrew/bin',
                             '/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin', '/usr/sbin', '/sbin']))
    if system == 'Darwin':
        return plistlib.dumps({
            'Label': LABEL, 'UserName': username,
            'ProgramArguments': [str(binary), '--config', str(config), 'daemon'],
            'WorkingDirectory': str(home), 'RunAtLoad': True, 'KeepAlive': True,
            'ThrottleInterval': 30, 'ExitTimeOut': 90, 'ProcessType': 'Background',
            'Umask': 0o077,
            'EnvironmentVariables': {'HOME': str(home), 'USER': username, 'LOGNAME': username, 'PATH': path},
            'StandardOutPath': str(home / '.codex-history-sync/v2/daemon.jsonl'),
            'StandardErrorPath': str(home / '.codex-history-sync/v2/daemon.stderr.log'),
        })
    if system != 'Linux':
        raise ValueError('Only macOS and systemd Linux are supported')

    def quoted(value):
        # systemd expands percent specifiers even in quoted values.
        return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%') + '"'

    return ('[Unit]\nDescription=Codex conversation history relay\n'
            'Wants=network-online.target\nAfter=network-online.target\nStartLimitIntervalSec=0\n\n'
            '[Service]\nType=simple\nUser=' + username + '\n'
            'WorkingDirectory=' + str(home).replace('%', '%%') + '\n'
            'Environment=' + quoted('HOME=' + str(home)) + '\n'
            'Environment=' + quoted('USER=' + username) + '\n'
            'Environment=' + quoted('LOGNAME=' + username) + '\n'
            'Environment=' + quoted('PATH=' + path) + '\n'
            'ExecStart=' + quoted(binary) + ' --config ' + quoted(config) + ' daemon\n'
            'Restart=always\nRestartSec=30\nTimeoutStopSec=90\nKillSignal=SIGTERM\n'
            'UMask=0077\nNoNewPrivileges=yes\n\n[Install]\nWantedBy=multi-user.target\n').encode()


def installer(home, username, uid, gid, system, stage, unit_file, binary_sha, config_sha, unit_sha, tmux,
              previous_sha=None):
    home, stage = Path(home), Path(stage)
    binary = home / '.local/bin/codex-history-sync'
    config = home / '.codex-history-sync/v2/config.json'
    q = shlex.quote
    target = ('/Library/LaunchDaemons/' + LABEL + '.plist' if system == 'Darwin'
              else '/etc/systemd/system/' + UNIT)
    pattern = '^' + re.escape(str(binary)) + '( --config ' + re.escape(str(config)) + ')? daemon$'
    backup = stage / 'previous-system-service'
    checksum = '/usr/bin/shasum -a 256' if system == 'Darwin' else '/usr/bin/sha256sum'
    target_guard = ('check_hash "$target" ' + q(previous_sha) + '\n' if previous_sha else
                    '[ ! -e "$target" ] || { echo "A service appeared after preparation; prepare again." >&2; exit 1; }\n')
    legacy_command = 'exec ' + q(str(binary)) + ' --config ' + q(str(config)) + ' daemon >> ' + \
        q(str(home / '.codex-history-sync/v2/daemon.jsonl')) + ' 2>> ' + \
        q(str(home / '.codex-history-sync/v2/daemon.stderr.log'))
    if system == 'Darwin':
        lint = '/usr/bin/plutil -lint "$candidate"\n'
        stop = '/bin/launchctl bootout system/' + LABEL + ' >/dev/null 2>&1 || true\n'
        publish = '/usr/bin/install -o root -g wheel -m 0644 "$candidate" "$target"\n'
        activate = '/bin/launchctl enable system/' + LABEL + '\n/bin/launchctl bootstrap system "$target"\n'
        restore = '/bin/launchctl bootstrap system "$target" >/dev/null 2>&1 || true\n'
        state = '/bin/launchctl print system/' + LABEL + ' > "$stage/service-state.txt"\n'
    else:
        lint = '/usr/bin/systemd-analyze verify "$candidate"\n'
        stop = '/usr/bin/systemctl stop ' + UNIT + ' >/dev/null 2>&1 || true\n'
        publish = '/usr/bin/install -o root -g root -m 0644 "$candidate" "$target"\n/usr/bin/systemctl daemon-reload\n'
        activate = '/usr/bin/systemctl enable --now ' + UNIT + '\n'
        restore = '/usr/bin/systemctl daemon-reload\n/usr/bin/systemctl start ' + UNIT + ' >/dev/null 2>&1 || true\n'
        state = '/usr/bin/systemctl show ' + UNIT + ' > "$stage/service-state.txt"\n'
    rollback_legacy = ''
    if tmux:
        rollback_legacy = 'if [ -n "$legacy" ]; then\n/usr/bin/sudo -u ' + q(username) + ' -- ' + \
            q(str(tmux)) + ' new-session -d -s codex-history-sync -c ' + q(str(home)) + ' ' + \
            q(legacy_command) + ' >/dev/null 2>&1 || true\nfi\n'
    return ('#!/bin/sh\nset -eu\numask 077\n'
            '[ "$(id -u)" -eq 0 ] || { echo "Run this prepared installer with sudo." >&2; exit 1; }\n'
            'candidate=' + q(str(unit_file)) + '\ntarget=' + q(target) + '\nstage=' + q(str(stage)) + '\n'
            'backup=' + q(str(backup)) + '\nlegacy=""\nchanged=0\n'
            'check_hash() { actual=$(' + checksum + ' "$1" | awk \'{print $1}\'); '
            '[ "$actual" = "$2" ] || { echo "Prepared file changed; prepare again: $1" >&2; exit 1; }; }\n'
            'check_hash "$candidate" ' + q(unit_sha) + '\n'
            'check_hash ' + q(str(binary)) + ' ' + q(binary_sha) + '\n'
            'check_hash ' + q(str(config)) + ' ' + q(config_sha) + '\n' + target_guard + lint +
            '[ ! -L "$target" ] || { echo "Refusing a symlink service definition." >&2; exit 1; }\n'
            'if [ -f "$target" ] && [ ! -f "$backup" ]; then cp -p "$target" "$backup"; fi\n'
            'rollback() {\ncode=$?\ntrap - EXIT\nif [ "$code" -ne 0 ] && [ "$changed" -eq 1 ]; then\n' + stop +
            'if [ -f "$backup" ]; then cp -p "$backup" "$target";\n' + restore +
            'else rm -f "$target";\n' + rollback_legacy +
            'fi\nprintf "failed %s\\n" "$code" > "$stage/install-result.txt"\nfi\nexit "$code"\n}\n'
            'trap rollback EXIT\nchanged=1\n' + stop +
            'legacy=$(pgrep -u ' + str(uid) + ' -f ' + q(pattern) + ' || true)\n'
            'for worker in $legacy; do kill -TERM "$worker"; done\n'
            'for worker in $legacy; do\nwaited=0\nwhile kill -0 "$worker" 2>/dev/null; do\n'
            'waited=$((waited + 1))\n[ "$waited" -le 90 ] || { echo "Old worker did not stop; refusing overlap." >&2; exit 1; }\n'
            'sleep 1\ndone\ndone\n' + publish + activate + state +
            'chown ' + str(uid) + ':' + str(gid) + ' "$stage/service-state.txt"\n'
            'printf "installed\\n" > "$stage/install-result.txt"\n'
            'chown ' + str(uid) + ':' + str(gid) + ' "$stage/install-result.txt"\n'
            'trap - EXIT\necho "History relay registered as a system service."\n').encode()


def prepare(home=None, system=None):
    if os.geteuid() == 0:
        raise ValueError('Prepare as the conversation owner, then use sudo for the prepared installer')
    home = Path(home or Path.home()).resolve()
    system = system or platform.system()
    owner = pwd.getpwuid(os.getuid())
    if not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]*', owner.pw_name):
        raise ValueError('Unsupported service account name')
    if '\n' in str(home) or '\r' in str(home):
        raise ValueError('Unsupported service home path')
    binary = home / '.local/bin/codex-history-sync'
    config = home / '.codex-history-sync/v2/config.json'
    cfg = json.loads(config.read_text())
    if not cfg.get('enabled') or Path(cfg.get('home', str(home / '.codex'))).resolve() != (home / '.codex').resolve():
        raise ValueError('Relay must be enabled and use this user canonical conversation home')
    store = Path(cfg.get('store', str(home / '.codex-history-sync/v2'))).resolve()
    if store != (home / '.codex-history-sync/v2').resolve():
        raise ValueError('Custom stores require separate service preparation')
    if not binary.is_file() or not os.access(binary, os.X_OK) or binary.stat().st_uid != os.getuid():
        raise ValueError('Installed relay executable must belong to the conversation owner')
    subprocess.run([str(binary), 'help'], stdout=subprocess.DEVNULL, check=True, timeout=15)
    target = Path('/Library/LaunchDaemons/' + LABEL + '.plist' if system == 'Darwin'
                  else '/etc/systemd/system/' + UNIT)
    previous_sha = None
    if target.exists():
        if target.is_symlink():
            raise ValueError('Refusing a symlink system service definition')
        raw = target.read_bytes()
        if system == 'Darwin':
            existing = plistlib.loads(raw)
            if existing.get('UserName') != owner.pw_name or existing.get('ProgramArguments', [])[:1] != [str(binary)]:
                raise ValueError('Existing system service belongs to another worker')
        elif ('\nUser=' + owner.pw_name + '\n').encode() not in raw or str(binary).encode() not in raw:
            raise ValueError('Existing system service belongs to another worker')
        previous_sha = digest(target)
    base = store / 'service'
    base.mkdir(mode=0o700, parents=True, exist_ok=True)
    stage = base / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    stage.mkdir(mode=0o700)
    filename = LABEL + '.plist' if system == 'Darwin' else UNIT
    unit_file = stage / filename
    unit_file.write_bytes(service_definition(home, owner.pw_name, system))
    unit_file.chmod(0o600)
    for name in ('daemon.jsonl', 'daemon.stderr.log'):
        path = store / name
        path.touch(exist_ok=True)
        path.chmod(0o600)
    tmux = shutil.which('tmux')
    script = stage / 'install-system.sh'
    script.write_bytes(installer(home, owner.pw_name, owner.pw_uid, owner.pw_gid, system, stage,
                                unit_file, digest(binary), digest(config), digest(unit_file), tmux, previous_sha))
    script.chmod(0o700)
    subprocess.run(['/bin/sh', '-n', str(script)], check=True)
    stable = base / 'install-system.sh'
    if stable.exists() or stable.is_symlink():
        stable.rename(stage / 'previous-installer.sh')
    stable.symlink_to(script)
    receipt = {'stage': str(stage), 'system': system, 'user': owner.pw_name, 'uid': owner.pw_uid,
               'binary_sha256': digest(binary), 'config_sha256': digest(config),
               'install_command': 'sudo ' + shlex.quote(str(stable)),
               'service': LABEL if system == 'Darwin' else UNIT}
    (stage / 'prepared.json').write_text(json.dumps(receipt, indent=2) + '\n')
    return receipt


if __name__ == '__main__':
    os.umask(0o077)
    print(json.dumps(prepare(), ensure_ascii=False, indent=2))
