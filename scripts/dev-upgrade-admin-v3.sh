#!/bin/bash
# Development-only upgrade of the explicitly authorized test server.
# Requires the previously staged, SHA256-pinned helper and source files.
set -euo pipefail
umask 077
[[ ${SBE_DEVELOPER:-0} == 1 && $EUID == 0 ]] || {
    printf '%s\n' 'This development upgrade requires root and SBE_DEVELOPER=1.' >&2
    exit 1
}
root=/etc/sing-box-enhanced
account=a-c8dc01e0691c4340
backup="$root/backups/admin-v3-$(date -u +%Y%m%dT%H%M%S)"
mkdir -m 0700 "$backup"
exec >"$backup/upgrade.log" 2>&1
printf '%s\n' "$backup" > /root/sbe-admin-upgrade-backup
cutover_started=0
failure() {
    status=$?
    if [[ $status != 0 && $cutover_started == 1 ]]; then
        systemctl stop sing-box-enhanced-meter.service || true
        systemctl stop "sbe-account-$account.service" || true
        printf '%s\n' "upgrade failed: $status; service stop requested; inspect both units" >"$backup/failed"
    fi
    exit "$status"
}
trap failure EXIT
systemctl is-active --quiet sing-box-enhanced-meter.service
systemctl is-active --quiet "sbe-account-$account.service"
[[ -x /root/sbe-linux-admin ]]
[[ -f /root/sbe-admin-source/src/enhanced.sh && -f /root/sbe-admin-source/scripts/sb-enhanced.sh ]]
[[ -f /root/sbe-accounts-before.sha256 ]]
[[ $(sha256sum /root/sbe-linux-admin | cut -d ' ' -f 1) == 2e1951e5d4dd905a82943e820548a77967ec24a98729359bc364e5b9a3cf9b14 ]]
bash -n /root/sbe-admin-source/src/enhanced.sh
bash -n /root/sbe-admin-source/scripts/sb-enhanced.sh
sha256sum -c /root/sbe-accounts-before.sha256
/usr/local/bin/sb account list >"$backup/before-account.json"
python3 - "$backup" <<'PY'
import hashlib,json,os,pathlib,sqlite3,sys
b=pathlib.Path(sys.argv[1]); r=pathlib.Path('/etc/sing-box-enhanced')
a=json.loads((b/'before-account.json').read_text())
assert len(a)==1 and a[0]['Account']['ID']=='a-c8dc01e0691c4340'
assert a[0]['Account']['Name']=='admin' and a[0]['Unlimited'] and a[0]['Allowed']
c=sqlite3.connect(r/'state.db'); assert c.execute('PRAGMA user_version').fetchone()[0]==2
identities=[list(row[:3])+[hashlib.sha256(row[3] if isinstance(row[3],bytes) else row[3].encode()).hexdigest()] for row in c.execute('SELECT id,account_id,node_id,inbound FROM bindings ORDER BY id')]
files=[pathlib.Path('/etc/sing-box/config.json'),pathlib.Path('/etc/sing-box/conf/Hysteria2-52613.json'),pathlib.Path('/etc/sing-box/bin/tls.cer'),pathlib.Path('/etc/sing-box/bin/tls.key')]
saved={'bindings':identities,'files':{str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in files}}
(b/'before-identities.json').write_text(json.dumps(saved))
PY
cutover_started=1
/usr/local/bin/sb service stop
systemctl stop sing-box-enhanced-meter.service
python3 - "$backup" <<'PY'
import pathlib,sqlite3,sys
b=pathlib.Path(sys.argv[1]); source=sqlite3.connect('/etc/sing-box-enhanced/state.db'); dest=sqlite3.connect(b/'state-v2.db')
source.backup(dest); dest.close(); source.close()
PY
cp -p "$root/bin/sbe" "$backup/sbe-v2"
cp -p "$root/sh/src/enhanced.sh" "$backup/enhanced.original"
cp -p "$root/sh/scripts/sb-enhanced.sh" "$backup/sb-enhanced.original"
install -m 0700 /root/sbe-linux-admin "$root/bin/sbe.next"
mv "$root/bin/sbe.next" "$root/bin/sbe"
install -m 0600 /root/sbe-admin-source/src/enhanced.sh "$root/sh/src/enhanced.sh.next"
mv "$root/sh/src/enhanced.sh.next" "$root/sh/src/enhanced.sh"
install -m 0700 /root/sbe-admin-source/scripts/sb-enhanced.sh "$root/sh/scripts/sb-enhanced.sh"
/usr/local/bin/sb admin setup --name admin >"$backup/admin-setup.result.json"
systemctl start sing-box-enhanced-meter.service
/usr/local/bin/sb account recover --id "$account"
/usr/local/bin/sb service start
systemctl is-active --quiet sing-box-enhanced-meter.service
systemctl is-active --quiet "sbe-account-$account.service"
systemctl is-active --quiet warp-svc.service
/usr/local/bin/sb account list >"$backup/after-account.json"
/usr/local/bin/sb admin show >"$backup/admin-show.json"
python3 - "$backup" <<'PY'
import hashlib,json,pathlib,sqlite3,stat,sys
b=pathlib.Path(sys.argv[1]); r=pathlib.Path('/etc/sing-box-enhanced')
before=json.loads((b/'before-account.json').read_text())[0]; after=json.loads((b/'after-account.json').read_text())[0]
assert after['Account']['ID']==before['Account']['ID'] and after['Unlimited'] and after['Allowed'] and not after['Account']['StatsFault']
c=sqlite3.connect(r/'state.db'); assert c.execute('PRAGMA user_version').fetchone()[0]==3
old_usage=c.execute('SELECT upload,download FROM usage WHERE account_id=? AND period=?',(before['Account']['ID'],before['Usage']['Period'])).fetchone()
assert old_usage is not None and old_usage[0]>=before['Usage']['Upload'] and old_usage[1]>=before['Usage']['Download']
saved=json.loads((b/'before-identities.json').read_text())
identities=[list(row[:3])+[hashlib.sha256(row[3] if isinstance(row[3],bytes) else row[3].encode()).hexdigest()] for row in c.execute('SELECT id,account_id,node_id,inbound FROM bindings ORDER BY id')]
assert identities==saved['bindings']
assert all(hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()==h for p,h in saved['files'].items())
admin=c.execute('SELECT account_id,password_hash FROM administrators WHERE id=1').fetchone()
assert admin[0]==before['Account']['ID'] and admin[1].startswith('$argon2id$v=19$m=65536,t=2,p=1$')
credential=r/'admin.initial-credentials.json'; secret=json.loads(credential.read_text())
assert secret['username']=='admin' and len(secret['password'])==43 and stat.S_IMODE(credential.stat().st_mode)==0o600
assert secret['password'] not in admin[1]
result={'passed':True,'schema':3,'admin_account_id':admin[0],'admin_name':'admin','credentials_file':str(credential),'credentials_mode':'0600','legacy_credentials_and_certificate_unchanged':True,'ledger_preserved':True,'unlimited_and_allowed':True,'login_api_enabled':False}
(b/'result.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result))
PY
sha256sum -c /root/sbe-accounts-before.sha256
