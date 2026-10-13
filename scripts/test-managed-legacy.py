#!/usr/bin/env python3
"""Verify an explicitly migrated Hysteria2 identity and a temporary WARP rule.

Run detached from SSH: adding/removing a rule restarts its account core.
Never disables the account or modifies its quota/credentials.
"""
import argparse
import http.server
import json
import os
import pathlib
import socket
import subprocess
import threading
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--root', required=True)
p.add_argument('--entry', required=True)
p.add_argument('--account', required=True)
p.add_argument('--template', required=True)
p.add_argument('--server-name', required=True)
a = p.parse_args()
os.umask(0o077)
root = pathlib.Path(a.root)
fixture = root / 'managed-legacy-verification'
fixture.mkdir(mode=0o700, exist_ok=True)
report = {'passed': False}
rule = 'www.cloudflare.com'
added = False
client = None


def sb(*words):
    r = subprocess.run([a.entry, *words], text=True, capture_output=True, timeout=40)
    if r.returncode:
        raise RuntimeError('managed command failed: ' + ' '.join(words[:2]))
    return json.loads(r.stdout) if r.stdout.strip() else None


def usage():
    return next(x for x in sb('account', 'list') if x['Account']['Name'] == a.account)


def private_json(path, data):
    path.write_text(json.dumps(data))
    path.chmod(0o600)


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        body = b'legacy-verification-' * 16384
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    original = json.loads(pathlib.Path(a.template).read_text())['inbounds'][0]
    assert original['type'] == 'hysteria2'
    state = usage()
    assert state['Allowed'] and state['Account']['Unlimited']
    before = state['Usage']
    identity = original['users'][0]
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        local_port = s.getsockname()[1]
    config = {
        'log': {'level': 'warn'},
        'inbounds': [{'type': 'socks', 'listen': '127.0.0.1', 'listen_port': local_port}],
        'outbounds': [{'type': 'hysteria2', 'server': '127.0.0.1', 'server_port': original['listen_port'],
                       'password': identity['password'],
                       'tls': {'enabled': True, 'server_name': a.server_name,
                               'certificate_path': original['tls']['certificate_path']}}],
    }
    private_json(fixture / 'client.json', config)
    with (fixture / 'client.log').open('w') as log:
        client = subprocess.Popen([str(root / 'bin/sing-box'), 'run', '-c', str(fixture / 'client.json')], stdout=log, stderr=log)
    time.sleep(1)
    curl = ['curl', '--noproxy', '', '--proxy', f'socks5h://127.0.0.1:{local_port}', '--max-time', '20', '-fsS']
    subprocess.run(curl + ['-o', '/dev/null', f'http://127.0.0.1:{server.server_port}/'], check=True, capture_output=True)
    report['old_credentials_and_trusted_certificate_work'] = True
    if rule not in (sb('warp', 'list')['domains'] or []):
        added = True
        sb('warp', 'add', '--domain', rule)
    time.sleep(1)
    trace = subprocess.run(curl + ['https://www.cloudflare.com/cdn-cgi/trace'], check=True, capture_output=True, text=True).stdout
    fields = dict(line.split('=', 1) for line in trace.splitlines() if '=' in line)
    assert fields.get('warp') == 'on'
    report['legacy_domain_route_uses_real_warp'] = True
    report['warp_location'] = fields.get('loc', '')
    time.sleep(1.5)
    after = usage()
    report['upload_delta'] = after['Usage']['Upload'] - before['Upload']
    report['download_delta'] = after['Usage']['Download'] - before['Download']
    assert report['upload_delta'] > 0 and report['download_delta'] >= 327680
    report['unlimited_account_still_metered'] = after['Allowed'] and after['Unlimited']
    report['passed'] = True
except Exception as e:
    report['error_type'] = type(e).__name__
    raise
finally:
    if client:
        client.terminate()
        client.wait(timeout=10)
    server.shutdown()
    try:
        if added:
            sb('warp', 'remove', '--domain', rule)
        report['temporary_rule_restored'] = not added or rule not in (sb('warp', 'list')['domains'] or [])
    except Exception as e:
        report['passed'] = False
        report['cleanup_error_type'] = type(e).__name__
        raise
    finally:
        private_json(fixture / 'result.json', report)
        print(json.dumps(report))
