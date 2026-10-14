#!/usr/bin/env python3
"""Root-only isolated smoke test; never touches the legacy sing-box unit.
Run on a test installation: python3 scripts/test-live.py --entry /usr/local/bin/sb-enhanced --root /opt/sing-box-enhanced-test
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

parser = argparse.ArgumentParser()
parser.add_argument('--entry', required=True)
parser.add_argument('--root', required=True)
args = parser.parse_args()
root = pathlib.Path(args.root)
fixture = root / 'live-fixtures'
fixture.mkdir(mode=0o700, exist_ok=True)
core = root / 'bin/sing-box'
report = []


def sb(*words):
    p = subprocess.run([args.entry, *words], text=True, capture_output=True, timeout=40)
    if p.returncode:
        raise RuntimeError(' '.join(words[:2]) + ': ' + p.stderr)
    return json.loads(p.stdout) if p.stdout.strip() else None


def private_json(path, value):
    path.write_text(json.dumps(value))
    path.chmod(0o600)


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        body = b'test-flow-' * 32768
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'accepted')


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
url = f'http://127.0.0.1:{server.server_port}/'
cert, key = fixture / 'tls.cer', fixture / 'tls.key'
subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=localhost', '-keyout', str(key), '-out', str(cert)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
key.chmod(0o600)
cert.chmod(0o600)
tls = {'enabled': True, 'certificate_path': str(cert), 'key_path': str(key)}
account = sb('account', 'create', '--name', 'smoke-main', '--bytes', '1073741824')['id']
other = sb('account', 'create', '--name', 'smoke-other', '--bytes', '1073741824')['id']
payload = fixture / 'upload.bin'
payload.write_bytes(b'upload-test-' * 16384)
payload.chmod(0o600)
protocols = ['socks', 'vmess', 'vless', 'trojan', 'shadowsocks', 'tuic', 'hysteria2', 'anytls']
try:
    for protocol in protocols:
        inbound = {'type': protocol, 'tag': 'template-' + protocol, 'listen': '127.0.0.1', 'listen_port': port()}
        if protocol in ('vmess', 'vless', 'tuic'):
            inbound['users'] = [{'uuid': 'f718a833-b54d-4a72-a27b-d02c9f211369'}]
            if protocol == 'tuic':
                inbound['users'][0]['password'] = 'template-password'
        elif protocol == 'shadowsocks':
            inbound.update(method='aes-128-gcm', password='template-password')
        elif protocol == 'socks':
            inbound['users'] = [{'username': 'template', 'password': 'template-password'}]
        else:
            inbound['users'] = [{'password': 'template-password'}]
        if protocol in ('trojan', 'tuic', 'hysteria2', 'anytls'):
            inbound['tls'] = tls
        template = fixture / (protocol + '.json')
        private_json(template, {'inbounds': [inbound]})
        sb('node', 'add', '--name', 'smoke-' + protocol, '--template', str(template))
        sb('node', 'bind', '--node', 'smoke-' + protocol, '--account', account)
        if protocol == 'socks':
            sb('node', 'bind', '--node', 'smoke-socks', '--account', other)
        binding = next(x['configuration']['inbounds'][0] for x in sb('node', 'endpoints', '--account', account) if x['name'] == 'smoke-' + protocol)
        outbound = {'type': protocol, 'tag': 'test-out', 'server': '127.0.0.1', 'server_port': binding['listen_port']}
        user = binding.get('users', [{}])[0]
        if protocol in ('vmess', 'vless', 'tuic'):
            outbound['uuid'] = user['uuid']
            if protocol == 'vmess':
                outbound['security'] = 'auto'
            if protocol == 'tuic':
                outbound['password'] = user['password']
                outbound['congestion_control'] = 'bbr'
        elif protocol == 'shadowsocks':
            outbound.update(method=binding['method'], password=binding['password'])
        elif protocol == 'socks':
            outbound.update(username=user['username'], password=user['password'])
        else:
            outbound['password'] = user['password']
        if protocol in ('trojan', 'tuic', 'hysteria2', 'anytls'):
            # Self-signed test certificate on loopback only.
            outbound['tls'] = {'enabled': True, 'insecure': True, 'server_name': 'localhost'}
        socks_port = port()
        client_file = fixture / ('client-' + protocol + '.json')
        private_json(client_file, {'log': {'level': 'warn'}, 'inbounds': [{'type': 'socks', 'tag': 'client', 'listen': '127.0.0.1', 'listen_port': socks_port}], 'outbounds': [outbound]})
        with (fixture / ('client-' + protocol + '.log')).open('w') as log:
            process = subprocess.Popen([str(core), 'run', '-c', str(client_file)], stdout=log, stderr=log)
            try:
                time.sleep(0.6)
                before = next(x for x in sb('account', 'list') if x['Account']['ID'] == account)['Usage']
                proxy = f'socks5h://127.0.0.1:{socks_port}'
                curl = ['curl', '--noproxy', '', '--proxy', proxy, '--max-time', '12', '-fsS', '-o', '/dev/null']
                subprocess.run(curl + [url], check=True, capture_output=True)
                subprocess.run(curl + ['--data-binary', '@' + str(payload), url], check=True, capture_output=True)
                time.sleep(1.5)
                after = next(x for x in sb('account', 'list') if x['Account']['ID'] == account)['Usage']
                up = after['Upload'] - before['Upload']
                down = after['Download'] - before['Download']
                assert up >= payload.stat().st_size and down >= 327680, (protocol, up, down)
                report.append({'protocol': protocol, 'upload_delta': up, 'download_delta': down, 'passed': True})
            finally:
                process.terminate()
                process.wait(timeout=10)
    # A quota stop must kill all inbounds of the account while preserving others.
    sb('account', 'quota', '--id', account, '--bytes', '1')
    time.sleep(0.5)
    statuses = sb('account', 'list')
    assert not next(x for x in statuses if x['Account']['ID'] == account)['Allowed']
    assert next(x for x in statuses if x['Account']['ID'] == other)['Allowed']
    state = subprocess.run(['systemctl', 'is-active', 'sbe-account-' + account + '.service'], text=True, capture_output=True)
    assert state.stdout.strip() == 'inactive', state.stdout
    assert subprocess.run(['systemctl', 'is-active', 'sbe-account-' + other + '.service'], capture_output=True).returncode == 0
    sb('account', 'quota', '--id', account, '--bytes', '1073741824')
    sb('account', 'reset', '--id', account)
    assert subprocess.run(['systemctl', 'is-active', 'sbe-account-' + account + '.service'], capture_output=True).returncode == 0
    sb('account', 'disable', '--id', account)
    sb('account', 'reset', '--id', account)
    assert not next(x for x in sb('account', 'list') if x['Account']['ID'] == account)['Allowed']
    report.append({'quota_all_inbounds_stop': True, 'other_account_unaffected': True, 'reset_resumes': True, 'manual_disable_survives_reset': True})
finally:
    for identifier in (account, other):
        try:
            sb('account', 'disable', '--id', identifier)
        except Exception:
            pass
    server.shutdown()
    private_json(fixture / 'report.json', report)
print(json.dumps(report, indent=2))
