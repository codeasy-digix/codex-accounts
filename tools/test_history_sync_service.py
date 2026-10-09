import hashlib
import plistlib
import subprocess
import tempfile
import unittest
from pathlib import Path

import history_sync_service as service


class ServiceDefinitionTests(unittest.TestCase):
    def test_macos_system_service_runs_as_owner_with_explicit_canonical_configuration(self):
        raw = service.service_definition('/Users/owner', 'owner', 'Darwin')
        cfg = plistlib.loads(raw)
        self.assertEqual(cfg['UserName'], 'owner')
        self.assertEqual(cfg['ProgramArguments'], ['/Users/owner/.local/bin/codex-history-sync',
                         '--config', '/Users/owner/.codex-history-sync/v2/config.json', 'daemon'])
        self.assertTrue(cfg['RunAtLoad'])
        self.assertTrue(cfg['KeepAlive'])
        self.assertNotIn('CODEX_HOME', cfg['EnvironmentVariables'])
        self.assertNotIn('SSH_AUTH_SOCK', cfg['EnvironmentVariables'])

    def test_systemd_paths_escape_percent_specifiers_and_spaces(self):
        cfg = service.service_definition('/home/owner test%profile', 'owner', 'Linux').decode()
        self.assertIn('User=owner\n', cfg)
        self.assertIn('WorkingDirectory=/home/owner test%%profile\n', cfg)
        self.assertIn('WantedBy=multi-user.target', cfg)
        self.assertIn('Restart=always', cfg)
        self.assertNotIn('CODEX_HOME=', cfg)

    def test_prepared_root_script_is_valid_shell_and_pins_inputs_before_stopping_workers(self):
        with tempfile.TemporaryDirectory() as name:
            home = Path(name) / "owner's space"
            for system in ('Darwin', 'Linux'):
                stage = home / 'prepared'
                filename = service.LABEL + '.plist' if system == 'Darwin' else service.UNIT
                sha = hashlib.sha256(b'fixture').hexdigest()
                raw = service.installer(home, 'owner', 501, 20, system, stage, stage / filename,
                                        sha, sha, sha, '/usr/bin/tmux')
                script = Path(name) / (system + '.sh')
                script.write_bytes(raw)
                subprocess.run(['/bin/sh', '-n', str(script)], check=True)
                text = raw.decode()
                self.assertLess(text.index('check_hash "$candidate"'), text.index('legacy=$(pgrep'))
                self.assertIn('kill -TERM "$worker"', text)
                self.assertNotIn('kill -KILL', text)
                self.assertIn('refusing overlap', text)
                self.assertIn('trap rollback EXIT', text)


if __name__ == '__main__':
    unittest.main()
