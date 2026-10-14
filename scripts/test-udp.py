#!/usr/bin/env python3
"""UDP byte metering for supported protocols in the test-live fixtures."""
import argparse,json,pathlib,socket,struct,subprocess,threading,time
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--root',required=True);p.add_argument('--entry',required=True);a=p.parse_args()
root=pathlib.Path(a.root);f=root/'live-fixtures';core=root/'bin/sing-box'
def sb(*words):
 r=subprocess.run([a.entry,*words],text=True,capture_output=True,timeout=30)
 if r.returncode:raise RuntimeError(r.stderr)
 return json.loads(r.stdout) if r.stdout.strip() else None
def used():
 x=next(x for x in sb('account','list') if x['Account']['Name']=='smoke-main')['Usage'];return x['Upload'],x['Download']
server=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);server.bind(('127.0.0.1',0));server.settimeout(1);stop=False
def echo():
 while not stop:
  try:data,peer=server.recvfrom(65535);server.sendto(data,peer)
  except socket.timeout:pass
threading.Thread(target=echo,daemon=True).start();report=[]
sb('account','recover','--id','smoke-main');sb('account','enable','--id','smoke-main')
try:
 for protocol in ('socks','shadowsocks','tuic','hysteria2','vmess','vless','trojan'):
  path=f/('client-'+protocol+'.json');cfg=json.load(open(path));listen=cfg['inbounds'][0]['listen_port']
  log=open(f/('udp-'+protocol+'.log'),'w');client=subprocess.Popen([str(core),'run','-c',str(path)],stdout=log,stderr=log)
  try:
   time.sleep(.5);before=used()
   tcp=socket.create_connection(('127.0.0.1',listen));tcp.settimeout(3);tcp.sendall(b'\x05\x01\x00');assert tcp.recv(2)==b'\x05\x00'
   tcp.sendall(b'\x05\x03\x00\x01'+b'\x00'*6);answer=tcp.recv(64);assert answer[:2]==b'\x05\x00',answer
   if answer[3]==1:host=socket.inet_ntoa(answer[4:8]);port=struct.unpack('!H',answer[8:10])[0]
   elif answer[3]==4:host=socket.inet_ntop(socket.AF_INET6,answer[4:20]);port=struct.unpack('!H',answer[20:22])[0]
   else:raise RuntimeError('unexpected SOCKS UDP bind type')
   if host in ('0.0.0.0','::'):host='127.0.0.1'
   udp=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);udp.settimeout(5)
   header=b'\x00\x00\x00\x01'+socket.inet_aton('127.0.0.1')+struct.pack('!H',server.getsockname()[1]);body=b'udp-meter-test-'*100
   for _ in range(5):
    udp.sendto(header+body,(host,port));data,_=udp.recvfrom(65535);assert data.endswith(body)
   udp.close();tcp.close();time.sleep(1.5);after=used();up,down=after[0]-before[0],after[1]-before[1];assert up>=len(body)*5 and down>=len(body)*5,(protocol,up,down)
   report.append({'protocol':protocol,'udp_upload':up,'udp_download':down,'passed':True})
  except Exception as e:report.append({'protocol':protocol,'error':str(e),'passed':False})
  finally:client.terminate();client.wait(timeout=5);log.close()
finally:
 stop=True;server.close();sb('account','disable','--id','smoke-main')
print(json.dumps(report,indent=2));raise SystemExit(0 if all(x['passed'] for x in report) else 1)
