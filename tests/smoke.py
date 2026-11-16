#!/usr/bin/env python3
"""Run the release and Caddy over local TLS against a loopback WAHA stub."""

import argparse
import base64
import contextlib
import datetime
import http.server
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.request


DOMAIN = "social.example.test"
ORIGIN = f"https://{DOMAIN}"
TOKEN = "smoke-test-operator-bearer-token-0000"
RECIPIENT = "44123456789@c.us"
PROJECT = Path(__file__).resolve().parents[1]


class WAHA(http.server.BaseHTTPRequestHandler):
    def reply(self, data, status=200):
        body = json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/api/sessions/default" and self.headers.get("X-Api-Key") == "stub-key":
            self.reply({"status": "WORKING"})
        else:
            self.reply({}, 404)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path != "/api/sendText" or body["chatId"] != RECIPIENT:
            self.reply({}, 400)
            return
        self.server.messages.append(body)
        self.reply({"id": "waha-stub-message"})

    def log_message(self, *args):
        pass


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(check, seconds=8):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except OSError:
            pass
        time.sleep(0.1)
    raise AssertionError("timed out waiting for smoke check")


@contextlib.contextmanager
def process(command, env, logfile):
    with logfile.open("ab") as logs:
        child = subprocess.Popen(command, env=env, stdout=logs, stderr=logs)
        try:
            yield child
        finally:
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()


def run_smoke(binary, caddy):
    native_port = free_port()
    tls_port = free_port()
    with tempfile.TemporaryDirectory() as root:
        scratch = Path(root)
        with http.server.ThreadingHTTPServer(("127.0.0.1", 0), WAHA) as provider:
            provider.messages = []
            thread = threading.Thread(target=provider.serve_forever, daemon=True)
            thread.start()
            try:
                exercise(binary, caddy, scratch, provider, native_port, tls_port)
            finally:
                provider.shutdown()
                thread.join()


def exercise(binary, caddy, scratch, provider, native_port, tls_port):
    env = os.environ | {
        "PUBLIC_BASE_URL": ORIGIN,
        "HTTP_LISTEN_ADDRESS": f"127.0.0.1:{native_port}",
        "SQLITE_PATH": str(scratch / "social.db"),
        "MEDIA_DIRECTORY": str(scratch / "media"),
        "SCHEDULER_POLL_INTERVAL": "100ms",
        "CREDENTIAL_ENCRYPTION_KEY": base64.b64encode(b"a" * 32).decode(),
        "WAHA_BASE_URL": f"http://127.0.0.1:{provider.server_port}",
        "WAHA_API_KEY": "stub-key",
        "WAHA_PREVIEW_HOSTS": "example.com",
        "SMOKE_OPERATOR_TOKEN": TOKEN,
        "LINKEDIN_CLIENT_ID": "mock-linkedin-app",
        "LINKEDIN_CLIENT_SECRET": "mock-linkedin-secret",
        "SOCIAL_ACCOUNTS": json.dumps([
            {"id": "wa", "platform": "whatsapp", "session": "default"},
            {"id": "li", "platform": "linkedin", "remote_id": "urn:li:person:abc"},
        ]),
        "SOCIAL_CLIENTS": json.dumps([{
            "id": "smoke", "token_env": "SMOKE_OPERATOR_TOKEN",
            "publish": True, "admin": True, "accounts": {"wa": [RECIPIENT], "li": []},
        }]),
        "XDG_DATA_HOME": str(scratch / "caddy-data"),
        "XDG_CONFIG_HOME": str(scratch / "caddy-config"),
    }
    caddyfile = scratch / "Caddyfile"
    config = (PROJECT / "deployments/Caddyfile").read_text()
    config = config.replace("social.example.com {", f"{DOMAIN} {{\n\ttls internal")
    config = config.replace("127.0.0.1:8080", env["HTTP_LISTEN_ADDRESS"])
    caddyfile.write_text(
        f"{{\n admin off\n skip_install_trust\n auto_https disable_redirects\n https_port {tls_port}\n}}\n" + config
    )
    ca = Path(env["XDG_DATA_HOME"]) / "caddy/pki/authorities/local/root.crt"

    def request(path, method="GET", data=None, auth=True, headers=()):
        command = [
            "curl", "--silent", "--show-error", "--noproxy", "*", "--max-time", "5",
            "--connect-to", f"{DOMAIN}:443:127.0.0.1:{tls_port}", "--cacert", str(ca),
            "--write-out", "\n%{http_code}", "-X", method,
        ]
        if auth:
            command += ["-H", f"Authorization: Bearer {TOKEN}"]
        if data is not None:
            command += ["-H", "Content-Type: application/json", "--data-binary", json.dumps(data)]
        for value in headers:
            command += ["-H", value]
        result = subprocess.run(command + [ORIGIN + path], capture_output=True, text=True)
        if result.returncode != 0:
            raise OSError(result.stderr)
        body, status = result.stdout.rsplit("\n", 1)
        return int(status), body

    def native_ready():
        with urllib.request.urlopen(f"http://{env['HTTP_LISTEN_ADDRESS']}/readyz", timeout=1) as response:
            return response.status == 200

    try:
        with process([str(caddy), "run", "--config", str(caddyfile)], env, scratch / "caddy.log"):
            with process([str(binary)], env, scratch / "app.log"):
                wait_for(native_ready)
                wait_for(lambda: ca.exists() and request("/mcp", auth=False)[0] == 401)
                assert request("/readyz")[0] == 404
                initialize = {
                    "jsonrpc": "2.0", "id": 1, "method": "initialize",
                    "params": {"protocolVersion": "2025-11-25", "capabilities": {},
                               "clientInfo": {"name": "smoke", "version": "1"}},
                }
                status, body = request("/mcp", "POST", initialize,
                                       headers=("Accept: application/json, text/event-stream",))
                assert status == 200 and "social-mcp" in body, (status, body)
                status, body = request("/connect/li", "POST")
                assert status == 201, (status, body)
                browser_url = json.loads(body)["url"]
                assert request(browser_url.removeprefix(ORIGIN), auth=False)[0] == 303
                assert request("/oauth/linkedin/callback?code=mock&state=bad", auth=False)[0] == 400

                due = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=4)
                schedule = {
                    "idempotency_key": "smoke-test-scheduled",
                    "scheduled_at": due.isoformat(),
                    "targets": [{"platform": "whatsapp", "account_id": "wa",
                                 "recipient_id": RECIPIENT, "url": "https://example.com/"}],
                }
                status, body = request("/api/schedule", "POST", schedule)
                assert status == 202, (status, body)
                delivery = json.loads(body)[0]["id"]
                assert len(provider.messages) == 0

            with sqlite3.connect(env["SQLITE_PATH"]) as db:
                assert db.execute("SELECT state FROM deliveries WHERE id=?", (delivery,)).fetchone() == ("scheduled",)

            with process([str(binary)], env, scratch / "app.log") as service:
                wait_for(native_ready)

                def published():
                    status, body = request(f"/api/deliveries/{delivery}")
                    if status != 200:
                        return False
                    record = json.loads(body)[0]["delivery"]
                    return record["state"] == "published" and record["external_id"] == "waha-stub-message"

                wait_for(published)
                assert len(provider.messages) == 1
                assert provider.messages[0]["linkPreview"] is True
                assert provider.messages[0]["linkPreviewHighQuality"] is True
                snapshot = scratch / "snapshot.db"
                subprocess.run([str(binary), "backup", str(snapshot)], env=env, check=True)
                with sqlite3.connect(snapshot) as db:
                    assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)
                    assert db.execute("SELECT state FROM deliveries WHERE id=?", (delivery,)).fetchone() == ("published",)

                memory = []
                for line in Path(f"/proc/{service.pid}/status").read_text().splitlines():
                    if line.startswith(("VmRSS:", "VmHWM:")):
                        memory.append(line)
                print("Native process after one mock delivery. " + "  ".join(memory))
                print("Smoke passed for TLS, MCP, provider OAuth routing, restart, preview flags and backup.")
    except Exception:
        for name in ("app.log", "caddy.log"):
            path = scratch / name
            if path.exists():
                print(path.read_text())
        raise


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default=PROJECT / "bin/social-mcp", type=Path)
    parser.add_argument("--caddy", default="caddy")
    args = parser.parse_args()
    run_smoke(args.binary.resolve(), args.caddy)
