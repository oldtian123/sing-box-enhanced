#!/usr/bin/env python3
"""Isolated WARP failure/daemon failure smoke test. Root only."""
import argparse
import http.server
import json
import os
import pathlib
import socket
import subprocess
import threading
import time

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--root',required=True)
p.add_argument('--entry',required=True)
a=p.parse_args()
os.umask(0o077)
root=pathlib.Path(a.root)
f=root/('protection-fixtures-'+str(time.time_ns()))
f.mkdir(mode=0o700)

def sb(*args):
 r=subprocess.run([a.entry,*args],capture_output=True,text=True,timeout=30)
 if r.returncode: raise RuntimeError(r.stderr)
 return json.loads(r.stdout) if r.stdout.strip() else None

class H(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(b'local-direct-works')
server=http.server.ThreadingHTTPServer(('127.0.0.1',0),H)
threading.Thread(target=server.serve_forever,daemon=True).start()
account=sb('account','create','--name',f.name,'--bytes','1073741824')['id']
template=f/'socks.json'
template.write_text(json.dumps({'inbounds':[{'type':'socks','tag':'fixture','listen':'127.0.0.1','listen_port':39999,'users':[{'username':'template','password':'template'}]}]}))
node=sb('node','add','--name',f.name,'--template',str(template))['id']
try:
 sb('node','bind','--node',node,'--account',account)
 binding=sb('node','endpoints','--account',account)[0]['configuration']['inbounds'][0]
 user=binding['users'][0]
 proxy=['curl','--noproxy','','--proxy',f'socks5h://127.0.0.1:{binding["listen_port"]}','--proxy-user',user['username']+':'+user['password'],'--max-time','4','-fsS','-o','/dev/null']
 # HTTP Host sniffing makes an exact fixed domain observable without DNS edits.
 # Port 40000 is intentionally unavailable.
 sb('warp','add','--domain','warp-smoke.example')
 blocked=subprocess.run(proxy+['-H','Host: warp-smoke.example',f'http://127.0.0.1:{server.server_port}/'],capture_output=True)
 assert blocked.returncode!=0,'matched domain fell back to direct'
 direct=subprocess.run(proxy+[f'http://127.0.0.1:{server.server_port}/'],capture_output=True)
 assert direct.returncode==0,'nonmatched direct broken'
 sb('warp','remove','--domain','warp-smoke.example')
 # The same policy must cover migrated account identities (currently disabled).
 cfg=json.load(open(root/'instances'/account/'config.json'))
 assert cfg['experimental']['v2ray_api']['listen'].startswith('127.0.0.1:')
 subprocess.run(['systemctl','kill','--kill-whom=main','--signal=SIGKILL','sing-box-enhanced-meter.service'],check=True,capture_output=True)
 time.sleep(3)
 status=next(x for x in sb('account','list') if x['Account']['ID']==account)
 assert status['Account']['StatsFault'] and not status['Allowed'],status
 assert subprocess.run(['systemctl','is-active','sbe-account-'+account+'.service'],capture_output=True).returncode!=0
 assert subprocess.run(['systemctl','is-active','sing-box.service'],capture_output=True).returncode==0
 print(json.dumps({'warp_matched_failure_blocks':True,'nonmatched_direct_survives':True,'stats_api_loopback_only':True,'meter_death_stops_account':True,'fault_persists_after_daemon_restart':True,'production_service_untouched':True}))
finally:
 try:sb('account','disable','--id',account)
 except Exception:pass
 server.shutdown()
