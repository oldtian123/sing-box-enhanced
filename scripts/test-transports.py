#!/usr/bin/env python3
"""Root-only transport matrix; uses upstream functions without init/main.

Requires an isolated enhanced test installation, its running meter, jq, curl,
and the TLS files already produced by test-live.py. No package installation,
Caddy changes, legacy service changes, or remote execution is performed here.
Managed test listeners use temporary dedicated ports and strong derived
credentials. TLS frontends owned by Caddy remain explicitly unverified.
"""
import argparse
import copy
import http.server
import json
import os
import pathlib
import re
import shutil
import socket
import ssl
import subprocess
import threading
import time
import uuid


def run(command, **kwargs):
    result = subprocess.run(command, text=True, capture_output=True, timeout=35, **kwargs)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip() or 'command failed')
    return result.stdout


def private_json(path, value):
    path.write_text(json.dumps(value, indent=2))
    path.chmod(0o600)


def port():
    with socket.socket() as tcp, socket.socket(type=socket.SOCK_DGRAM) as udp:
        tcp.bind(('127.0.0.1', 0))
        number = tcp.getsockname()[1]
        udp.bind(('127.0.0.1', number))
        return number


def client_transport(server_transport):
    transport = copy.deepcopy(server_transport)
    if transport.get('type') == 'httpupgrade':
        # v1.14.3 NewClient sets Request.Host from options.Host, while copying
        # Headers verbatim. A lowercase headers.host can emit a second Host
        # header and cause HTTP 400; move it into the dedicated client field.
        for name in list(transport.get('headers', {})):
            if name.lower() == 'host':
                host = transport['headers'].pop(name)
                if isinstance(host, list):
                    host = host[0] if host else ''
                if host and not transport.get('host'):
                    transport['host'] = host
    return transport


class Handler(http.server.BaseHTTPRequestHandler):
    body = b'transport-flow-' * 16384

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Length', str(len(self.body)))
        self.end_headers()
        self.wfile.write(self.body)

    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'accepted')


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--entry', required=True)
parser.add_argument('--root', required=True)
parser.add_argument('--source', default=str(pathlib.Path(__file__).resolve().parents[1] / 'src/core.sh'))
parser.add_argument('--cert')
parser.add_argument('--key')
parser.add_argument('--only', nargs='+', help='run selected exact variant names, comma-separated or space-separated')
args = parser.parse_args()
if os.geteuid() != 0:
    parser.error('run as root on an isolated enhanced test installation')
if any(shutil.which(tool) is None for tool in ('bash', 'jq', 'shuf', 'curl')):
    parser.error('existing bash, jq, shuf and curl are required; this test installs nothing')
if int(run(['bash', '-c', 'printf "%s" "${BASH_VERSINFO[0]}"'])) < 4:
    parser.error('upstream template generator requires Bash 4 or later')
os.umask(0o077)
root, source = pathlib.Path(args.root).resolve(), pathlib.Path(args.source).resolve()
core = root / 'bin/sing-box'
cert = pathlib.Path(args.cert or root / 'live-fixtures/tls.cer').resolve()
key = pathlib.Path(args.key or root / 'live-fixtures/tls.key').resolve()
if not all(path.is_file() for path in (core, source, cert, key)):
    parser.error('core, upstream src/core.sh and existing test TLS files are required')
fixture = root / ('transport-fixtures-' + str(time.time_ns()))
fixture.mkdir(mode=0o700)
report = {'core': run([str(core), 'version']).splitlines()[0],
          'source': str(source), 'rows': [], 'cleanup_errors': []}


def sb(*words):
    value = run([args.entry, *words])
    return json.loads(value) if value.strip() else None


def usage(account):
    return next(row['Usage'] for row in sb('account', 'list') if row['Account']['ID'] == account)


def check(path):
    result = subprocess.run([str(core), 'check', '-c', str(path)], text=True,
                            capture_output=True, timeout=20)
    return result.returncode == 0, result.stderr.strip()[:1000]


# Source only the trusted upstream function definitions. Never load init.sh,
# invoke add/main, issue certificates, install Caddy, or restart services.
generator = r'''
. "$1"
get_ip() { ip=127.0.0.1; }
err() { printf '%s\n' "$*" >&2; exit 1; }
is_core_bin=$3; is_tls_cer=$4; is_tls_key=$5
uuid=$6; password=$6; port=34567; path=/transport-test
is_private_key=$7; is_public_key=$8; is_servername=localhost
ss_method=${9:-}
is_test_json=1
[[ ${2,,} == *-tls ]] && host=localhost
create server "$2"
printf '%s\n' "$is_new_json"
'''
variants = run(['bash', '-c', '. "$1"; printf "%s\n" "${protocol_list[@]}"', 'sbe-test', str(source)]).splitlines()
variants = [name for name in variants if name.startswith(('VMess-', 'VLESS-', 'Trojan-'))]
methods = run(['bash', '-c', '. "$1"; printf "%s\n" "${ss_method_list[@]}"', 'sbe-test', str(source)]).splitlines()
specifications = [(name, name, '') for name in variants]
specifications += [('Shadowsocks/' + method, 'Shadowsocks', method) for method in methods]
if args.only:
    selected = {name.strip() for group in args.only for name in group.split(',') if name.strip()}
    known = {name for name, _, _ in specifications}
    if not selected or selected - known:
        parser.error('unknown/empty --only variants: ' + ', '.join(sorted(selected - known)))
    report['scope'] = 'selected_variants_only'
    report['excluded_variants'] = [name for name, _, _ in specifications if name not in selected]
    specifications = [item for item in specifications if item[0] in selected]
else:
    report['scope'] = 'all_upstream_transport_and_shadowsocks_variants'
report['rows'] = [{'variant': name, 'template_check': 'not_run', 'managed_binding': False,
                   'local_flow': 'not_run', 'quota_meter': 'not_run',
                   'caddy_tls_frontend': 'unverified' if name.endswith('-TLS') else 'not_required'}
                  for name, _, _ in specifications]
pair = run([str(core), 'generate', 'reality-keypair'])
keys = re.findall(r':\s*(\S+)', pair)
if len(keys) != 2:
    raise RuntimeError('cannot parse core-generated REALITY key pair')
target = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
camouflage = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.minimum_version = ssl.TLSVersion.TLSv1_3
context.set_alpn_protocols(['h2', 'http/1.1'])
context.load_cert_chain(str(cert), str(key))
camouflage.socket = context.wrap_socket(camouflage.socket, server_side=True)
for server in (target, camouflage):
    threading.Thread(target=server.serve_forever, daemon=True).start()
payload = fixture / 'upload.bin'
payload.write_bytes(b'upload-' * 16384)
account, nodes = None, []
try:
    if sb('service', 'status')['paused']:
        raise RuntimeError('test installation is paused; start its service explicitly before this test')
    account = sb('account', 'create', '--name', fixture.name, '--bytes', '1073741824')['id']
    for number, (variant, protocol, method) in enumerate(specifications):
        row = report['rows'][number]
        node = process = None
        try:
            template = json.loads(run(['bash', '-c', generator, 'sbe-test', str(source), protocol,
                                       str(core), str(cert), str(key), str(uuid.uuid4()), *keys, method]))
            raw_file = fixture / (str(number) + '-upstream.json')
            private_json(raw_file, template)
            row['template_check'], error = check(raw_file)
            if not row['template_check']:
                row['error'] = error
                continue
            inbound = template['inbounds'][0]
            # Test the backend on its own dedicated account port. This does not
            # claim validation of the original external Caddy hostname/route.
            inbound['listen'] = '::'
            tls = inbound.get('tls', {})
            if 'acme' in tls or tls.get('certificate_provider', {}).get('type') == 'acme':
                tls.pop('acme', None)
                tls.pop('certificate_provider', None)
                tls.update(certificate_path=str(cert), key_path=str(key))
            if tls.get('reality', {}).get('enabled'):
                tls['reality']['handshake'] = {'server': '127.0.0.1', 'server_port': camouflage.server_port}
            template_file = fixture / (str(number) + '-template.json')
            private_json(template_file, template)
            node = sb('node', 'add', '--name', fixture.name + '-' + str(number), '--template', str(template_file))['id']
            nodes.append(node)
            sb('node', 'bind', '--node', node, '--account', account, '--port', str(port()))
            row['managed_binding'] = True
            binding = next(item['configuration']['inbounds'][0]
                           for item in sb('node', 'endpoints', '--account', account)
                           if item['name'] == fixture.name + '-' + str(number))
            outbound = {'type': binding['type'], 'tag': 'test', 'server': '127.0.0.1',
                        'server_port': binding['listen_port']}
            user = binding.get('users', [{}])[0]
            for field in ('uuid', 'password', 'flow'):
                if field in user:
                    outbound[field] = user[field]
            if binding['type'] == 'shadowsocks':
                outbound.update(method=binding['method'], password=binding['password'])
            if binding['type'] == 'vmess':
                outbound['security'] = 'auto'
            if binding.get('transport'):
                outbound['transport'] = client_transport(binding['transport'])
            if binding.get('tls', {}).get('enabled'):
                outbound['tls'] = {'enabled': True, 'server_name': 'localhost', 'insecure': True}
                if 'alpn' in binding['tls']:
                    outbound['tls']['alpn'] = binding['tls']['alpn']
                if binding['tls'].get('reality', {}).get('enabled'):
                    outbound['tls'].pop('insecure')
                    outbound['tls'].update(utls={'enabled': True, 'fingerprint': 'chrome'},
                                           reality={'enabled': True, 'public_key': keys[1], 'short_id': ''})
            socks_port = port()
            client_file = fixture / (str(number) + '-client.json')
            private_json(client_file, {'log': {'level': 'warn'}, 'inbounds': [
                {'type': 'socks', 'listen': '127.0.0.1', 'listen_port': socks_port}], 'outbounds': [outbound]})
            ok, error = check(client_file)
            if not ok:
                raise RuntimeError('client check: ' + error)
            with (fixture / (str(number) + '-client.log')).open('w') as log:
                process = subprocess.Popen([str(core), 'run', '-c', str(client_file)], stdout=log, stderr=log)
                time.sleep(0.7)
                before = usage(account)
                curl = ['curl', '--noproxy', '', '--proxy', f'socks5h://127.0.0.1:{socks_port}',
                        '--max-time', '10', '-fsS', '-o', '/dev/null']
                url = f'http://127.0.0.1:{target.server_port}/'
                run(curl + [url])
                run(curl + ['--data-binary', '@' + str(payload), url])
                row['local_flow'] = 'passed'
                time.sleep(1.5)
                after = usage(account)
                row['upload_delta'] = after['Upload'] - before['Upload']
                row['download_delta'] = after['Download'] - before['Download']
                row['quota_meter'] = 'passed' if (row['upload_delta'] >= payload.stat().st_size
                                                  and row['download_delta'] >= len(Handler.body)) else 'failed'
        except Exception as error:
            row['error'] = str(error)[:1000]
        finally:
            if process:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            if node:
                try:
                    sb('node', 'unbind', '--node', node, '--account', account)
                except Exception as error:
                    report['cleanup_errors'].append(str(error)[:500])
                    sb('account', 'disable', '--id', account)
                    break
finally:
    if account:
        try:
            sb('account', 'disable', '--id', account)
        except Exception as error:
            report['cleanup_errors'].append('account disable: ' + str(error)[:500])
        # Final independent stop addresses a failed CLI/database cleanup too.
        try:
            stopped = subprocess.run(['systemctl', 'stop', 'sbe-account-' + account + '.service'],
                                     text=True, capture_output=True, timeout=20)
            if stopped.returncode:
                report['cleanup_errors'].append('independent account stop: ' + stopped.stderr[:500])
        except Exception as error:
            report['cleanup_errors'].append('independent account stop: ' + str(error)[:500])
    for server in (target, camouflage):
        server.shutdown()
        server.server_close()
    private_json(fixture / 'report.json', report)
print(json.dumps(report, indent=2))
raise SystemExit(1 if report['cleanup_errors'] or any(
    row['template_check'] is not True or row['quota_meter'] != 'passed' for row in report['rows']) else 0)
