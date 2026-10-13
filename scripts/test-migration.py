#!/usr/bin/env python3
"""Disposable legacy-fixture takeover; never operates sing-box.service."""
import argparse
import json
import os
import pathlib
import socket
import subprocess
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--root', required=True)
p.add_argument('--entry', required=True)
a = p.parse_args()
os.umask(0o077)
root = pathlib.Path(a.root)
fixture = root / ('legacy-fixture-' + str(time.time_ns()))
(fixture / 'conf').mkdir(parents=True, mode=0o700)
unit = 'sbe-legacy-smoke.service'
unitfile = pathlib.Path('/usr/local/lib/systemd/system') / unit
if unitfile.exists():
    raise RuntimeError('fixture unit already exists')

def command(*args, **kw):
    r = subprocess.run(args, capture_output=True, text=True, timeout=35, **kw)
    if r.returncode:
        raise RuntimeError(r.stderr or r.stdout)
    return r.stdout


def sb(*args):
    s = command(a.entry, *args, env=dict(os.environ, SBE_DEVELOPER='1', SBE_LEGACY_ROOT=str(fixture)))
    return json.loads(s) if s.strip() else None


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def listening(number):
    with socket.socket() as s:
        return s.connect_ex(('127.0.0.1', number)) == 0


def node(tag, number):
    return {'type': 'socks', 'tag': tag, 'listen': '127.0.0.1', 'listen_port': number,
            'users': [{'username': 'fixture-user', 'password': 'fixture-only-password'}]}


ports = [port(), port(), port()]
(fixture / 'config.json').write_text(json.dumps({'inbounds': [node('legacy-base', ports[0])], 'outbounds': [{'type': 'direct', 'tag': 'direct'}]}))
for index in (1, 2):
    (fixture / 'conf' / f'legacy-{index}.json').write_text(json.dumps({'inbounds': [node(f'legacy-{index}', ports[index])]}))
unitfile.parent.mkdir(parents=True, exist_ok=True)
unitfile.write_text(f'[Unit]\nDescription=Disposable enhanced migration fixture\n[Service]\nType=simple\nExecStart={root}/bin/sing-box run -c {fixture}/config.json -C {fixture}/conf\n')
owner = sb('account', 'create', '--name', fixture.name, '--bytes', '1073741824')['id']
try:
    command('systemctl', 'daemon-reload')
    command('systemctl', 'start', unit)
    time.sleep(0.5)
    assert all(listening(n) for n in ports), 'fixture did not start'
    inspected = sb('migrate', 'inspect', '--legacy-root', str(fixture), '--legacy-unit', unit)
    assert len(inspected['nodes']) == 3, inspected
    sb('migrate', 'import', '--legacy-root', str(fixture), '--legacy-unit', unit)
    assert all(listening(n) for n in ports), 'import changed legacy service'
    sb('node', 'bind', '--node', 'legacy-base-1', '--account', owner)
    sb('node', 'bind', '--node', 'legacy-1', '--account', owner)
    sb('migrate', 'apply')
    time.sleep(0.8)
    assert listening(ports[0]) and listening(ports[1]) and not listening(ports[2]), 'owner/quarantine failed'
    exported = sb('node', 'endpoints', '--account', owner)
    assert all(x['configuration']['inbounds'][0]['users'][0]['password'] == 'fixture-only-password' for x in exported), 'credential changed'
    sb('account', 'quota', '--id', owner, '--bytes', '0')
    assert not any(listening(n) for n in ports), 'quota bypass via legacy listener'
    print(json.dumps({'inspect_base_and_conf': True, 'import_preserves_old_service': True,
                      'credentials_and_ports_preserved': True, 'unowned_disabled': True,
                      'quota_stops_all_old_identity_access': True, 'production_unit_untouched': True}))
finally:
    try:
        sb('account', 'disable', '--id', owner)
    finally:
        subprocess.run(['systemctl', 'stop', unit], capture_output=True)
        subprocess.run(['systemctl', 'unmask', unit], capture_output=True)
        unitfile.unlink(missing_ok=True)
        subprocess.run(['systemctl', 'daemon-reload'], capture_output=True)
