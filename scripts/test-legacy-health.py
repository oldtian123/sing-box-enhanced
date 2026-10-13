#!/usr/bin/env python3
"""Test the installed legacy Hy2 identity without changing managed services."""
import argparse
import http.server
import ipaddress
import json
import os
import pathlib
import re
import shutil
import signal
import socket
import stat
import subprocess
import tempfile
import threading
import time

DOWNLOAD = (b"legacy-health-download\n" * 13000)[:256 * 1024]
UPLOAD = (b"legacy-health-upload\n" * 4000)[:64 * 1024]


class HealthError(Exception):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default="/etc/sing-box-enhanced")
    parser.add_argument("--entry", default="/usr/local/bin/sb")
    parser.add_argument("--account", default="admin")
    parser.add_argument("--template", default="/etc/sing-box/conf/Hysteria2-52613.json")
    parser.add_argument("--server-name")
    parser.add_argument("--server", choices=("127.0.0.1", "45.8.173.97"), default="127.0.0.1")
    parser.add_argument("--save-result", action="store_true")
    args = parser.parse_args()
    started = time.monotonic()
    deadline = started + 27
    report = {"passed": False, "stage": "preflight", "checks": {}}
    client = server = server_thread = temporary = None
    os.umask(0o077)

    def remaining(limit):
        seconds = min(limit, deadline - time.monotonic())
        if seconds <= 0:
            raise HealthError("total_timeout")
        return seconds

    def command(words, data=None, limit=4, check=True):
        result = subprocess.run(words, input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=remaining(limit))
        if check and result.returncode:
            raise HealthError("command_failed")
        return result

    def account():
        states = json.loads(command([args.entry, "account", "list"]).stdout)
        matches = [item for item in states if args.account in
                   (item["Account"]["ID"], item["Account"]["Name"])]
        if len(matches) != 1:
            raise HealthError("account_not_unique")
        state = matches[0]
        if state["Allowed"] is not True or state["Account"]["Unlimited"] is not True or state["Account"]["StatsFault"] is not False:
            raise HealthError("account_not_allowed_unlimited_and_fault_free")
        return state

    def services(account_id):
        units = {"meter": "sing-box-enhanced-meter.service",
                 "account_core": "sbe-account-" + account_id + ".service",
                 "warp": "warp-svc.service"}
        return {name: command(["/usr/bin/systemctl", "is-active", "--quiet", unit],
                              limit=1, check=False).returncode == 0
                for name, unit in units.items()}

    def private_write(path, data):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
        with os.fdopen(fd, "wb") as file:
            if not stat.S_ISREG(os.fstat(file.fileno()).st_mode):
                raise HealthError("private_output_must_be_regular")
            os.fchmod(file.fileno(), 0o600)
            file.write(data)

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def reply(self, status, body):
            self.send_response(status)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            self.reply(200, DOWNLOAD) if self.path == "/download" else self.reply(404, b"")

        def do_POST(self):
            self.connection.settimeout(2)
            valid = (self.path == "/upload" and
                     self.headers.get("Content-Length") == str(len(UPLOAD)) and
                     self.rfile.read(len(UPLOAD)) == UPLOAD)
            self.reply(200 if valid else 400, b"upload-ok" if valid else b"invalid")

    def interrupted(*_):
        raise HealthError("interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        if os.geteuid() != 0 or os.path.abspath(args.entry) != "/usr/local/bin/sb":
            raise HealthError("root_and_installed_sb_required")
        root = pathlib.Path(args.root)
        core = root / "bin/sing-box"
        if not core.is_file() or not os.access(core, os.X_OK):
            raise HealthError("installed_core_missing")
        before = account()
        account_id = before["Account"]["ID"]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", account_id):
            raise HealthError("invalid_account_id")
        report["services_before"] = services(account_id)
        report["checks"]["admin_allowed_unlimited_fault_free_before"] = True
        report["stage"] = "legacy_configuration"
        original = json.loads(pathlib.Path(args.template).read_bytes())
        candidates = [item for item in original["inbounds"] if item.get("type") == "hysteria2"]
        if len(candidates) != 1:
            raise HealthError("expected_one_legacy_hysteria2_inbound")
        inbound = candidates[0]
        password = inbound["users"][0]["password"]
        port = inbound["listen_port"]
        tls = inbound["tls"]
        server_name = args.server_name or tls.get("server_name")
        if not isinstance(password, str) or not password or not server_name or not isinstance(port, int) or not 1 <= port <= 65535:
            raise HealthError("legacy_credentials_port_or_tls_name_missing")
        certificate = pathlib.Path(tls["certificate_path"])
        if not certificate.is_absolute():
            raise HealthError("legacy_certificate_path_must_be_absolute")
        cert_data = certificate.read_bytes()
        if len(cert_data) > 1024 * 1024 or b"-----BEGIN CERTIFICATE-----" not in cert_data:
            raise HealthError("legacy_certificate_invalid")
        listening = command(["/usr/bin/ss", "-H", "-lun"], limit=1).stdout.decode()
        report["checks"]["legacy_udp_port_listening"] = bool(re.search(r":" + str(port) + r"\b", listening))
        temporary = pathlib.Path(tempfile.mkdtemp(prefix="sbe-legacy-health-"))
        temporary.chmod(0o700)
        private_write(temporary / "certificate.pem", cert_data)
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            socks_port = reserved.getsockname()[1]
        outbound = {"type": "hysteria2", "tag": "legacy", "server": args.server,
                    "server_port": port, "password": password,
                    "tls": {"enabled": True, "server_name": server_name,
                            "certificate_path": str(temporary / "certificate.pem")}}
        if inbound.get("obfs"):
            outbound["obfs"] = inbound["obfs"]
        config = {"log": {"level": "warn"}, "inbounds": [{"type": "socks", "listen": "127.0.0.1", "listen_port": socks_port}],
                  "outbounds": [outbound], "route": {"final": "legacy"}}
        private_write(temporary / "client.json", json.dumps(config).encode())
        report["stage"] = "temporary_client"
        with (temporary / "client.log").open("wb") as log:
            client = subprocess.Popen([str(core), "run", "-c", str(temporary / "client.json")],
                                      stdout=log, stderr=log, start_new_session=True)
        ready_until = time.monotonic() + remaining(3)
        while True:
            if client.poll() is not None:
                raise HealthError("temporary_client_exited")
            try:
                with socket.create_connection(("127.0.0.1", socks_port), timeout=remaining(0.2)):
                    break
            except OSError:
                if time.monotonic() >= ready_until:
                    raise HealthError("temporary_socks_not_ready")
                time.sleep(min(0.05, remaining(0.05)))
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.handle_error = lambda *_: None
        server_thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        server_thread.start()
        curl = ["/usr/bin/curl", "-q", "--silent", "--show-error", "--fail", "--noproxy", "",
                "--proxy", "socks5h://127.0.0.1:" + str(socks_port), "--connect-timeout", "3"]

        def transfer(url, data=None, maximum=256 * 1024):
            words = curl + ["--max-time", str(remaining(6)), "--max-filesize", str(maximum)]
            if data is not None:
                words += ["--data-binary", "@-"]
            if url.startswith("https:"):
                words += ["--proto", "=https", "--cacert", "/etc/ssl/certs/ca-certificates.crt"]
            return command(words + [url], data=data, limit=6).stdout

        endpoint = "http://127.0.0.1:" + str(server.server_port)
        report["stage"] = "legacy_download"
        if transfer(endpoint + "/download") != DOWNLOAD:
            raise HealthError("download_body_mismatch")
        report["checks"]["legacy_tls_download_256kib"] = True
        report["stage"] = "legacy_upload"
        if transfer(endpoint + "/upload", UPLOAD, maximum=1024) != b"upload-ok":
            raise HealthError("upload_body_mismatch")
        report["checks"]["legacy_tls_upload_64kib"] = True
        report["stage"] = "https_trace"
        trace = transfer("https://www.cloudflare.com/cdn-cgi/trace", maximum=16384).decode()
        fields = dict(line.split("=", 1) for line in trace.splitlines() if "=" in line)
        ipaddress.ip_address(fields["ip"])
        if not re.fullmatch(r"[A-Z]{2}", fields.get("loc", "")) or fields.get("warp") not in ("on", "plus", "off"):
            raise HealthError("invalid_https_trace")
        report["trace"] = {key: fields[key] for key in ("ip", "loc", "warp")}
        report["checks"]["https_through_legacy_tls_verified"] = True
        report["stage"] = "accounting"
        while True:
            after = account()
            if after["Account"]["ID"] != account_id or after["Usage"]["Period"] != before["Usage"]["Period"]:
                raise HealthError("account_or_usage_period_changed")
            upload = after["Usage"]["Upload"] - before["Usage"]["Upload"]
            download = after["Usage"]["Download"] - before["Usage"]["Download"]
            if upload >= len(UPLOAD) and download >= len(DOWNLOAD):
                break
            time.sleep(remaining(0.25))
        report["aggregate_account_delta"] = {"upload": upload, "download": download}
        report["checks"]["admin_allowed_unlimited_fault_free_after"] = True
        report["services_after"] = services(account_id)
        if not all(report["services_after"].values()):
            raise HealthError("required_service_not_active")
        report["passed"], report["stage"] = True, "complete"
    except Exception as error:
        report["error_code"] = str(error) if isinstance(error, HealthError) else type(error).__name__
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_ok = True
        if client is not None:
            try:
                if client.poll() is None:
                    os.killpg(client.pid, signal.SIGTERM)
                    try:
                        client.wait(timeout=1)
                    except subprocess.TimeoutExpired:
                        os.killpg(client.pid, signal.SIGKILL)
                        client.wait(timeout=1)
            except ProcessLookupError:
                pass
            except Exception:
                cleanup_ok = False
        if server is not None:
            try:
                if server_thread is not None and server_thread.is_alive():
                    server.shutdown()
                server.server_close()
            except Exception:
                cleanup_ok = False
        if temporary is not None:
            try:
                shutil.rmtree(temporary)
            except Exception:
                cleanup_ok = False
        report["temporary_cleanup_complete"] = cleanup_ok
        if not cleanup_ok:
            report["passed"], report["stage"], report["error_code"] = False, "cleanup", "temporary_cleanup_failed"
        report["elapsed_seconds"] = round(time.monotonic() - started, 3)
        if args.save_result:
            try:
                result_dir = pathlib.Path(args.root) / "legacy-health-verification"
                result_dir.mkdir(mode=0o700, exist_ok=True)
                info = result_dir.lstat()
                if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
                    raise HealthError("result_directory_not_private")
                report["result_saved"] = True
                private_write(result_dir / "result.json", json.dumps(report).encode())
            except Exception:
                report["result_saved"] = False
        print(json.dumps(report, sort_keys=True))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
