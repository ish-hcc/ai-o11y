"""Fake ai-o11y for the install script tests.

Answers the routes install.sh and uninstall.sh call, the way ai-o11y does:
POST /aio11y/targets registers (again) and answers the target, GET
.../collectorConfig answers the file FAKE_CONFIG names, DELETE answers 404 for
a target not registered. Every request is appended to FAKE_LOG/server.log.

    python3 -I fake_server.py <port file>
"""

import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = os.path.join(os.environ["FAKE_LOG"], "server.log")
TOKEN = os.environ["FAKE_TOKEN"]
targets = {}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def record(self, body=""):
        auth = self.headers.get("Authorization", "")
        with open(LOG, "a") as f:
            f.write(f"{self.command} {self.path} auth={auth} {body}\n")

    def answer(self, code, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def authorized(self):
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            self.answer(401, {"message": "Unauthorized"})
            return False
        return True

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
        self.record(body)
        if not self.authorized():
            return
        if self.path != "/aio11y/targets":
            return self.answer(404, {"message": "Not Found"})
        req = json.loads(body)
        if req.get("kind") != "node":
            return self.answer(400, {"message": "kind is required: node"})
        uid = "uid-" + req["nodeId"]
        targets[uid] = req
        attrs = {"nsId": req["nsId"], "infraId": req["infraId"], "nodeId": req["nodeId"]}
        self.answer(200, {"id": uid, "kind": "vm", "name": req["nodeId"], "provider": "azure",
                          "region": "koreacentral", "os": "linux", "gpuCount": 0, "gpuModel": "",
                          "attributes": attrs})

    def do_GET(self):
        self.record()
        if not self.authorized():
            return
        m = re.fullmatch(r"/aio11y/targets/([^/]+)/collectorConfig", self.path)
        if not m or m.group(1) not in targets:
            return self.answer(404, {"message": "Target is not registered"})
        with open(os.environ["FAKE_CONFIG"], "rb") as f:
            self.answer(200, f.read(), "application/yaml")

    def do_DELETE(self):
        self.record()
        if not self.authorized():
            return
        m = re.fullmatch(r"/aio11y/targets/([^/]+)", self.path)
        if not m or targets.pop(m.group(1), None) is None:
            return self.answer(404, {"message": "Target is not registered"})
        self.answer(200, {"message": "Target deleted"})


server = HTTPServer(("localhost", 0), Handler)
with open(sys.argv[1] + ".tmp", "w") as f:
    f.write(str(server.server_port))
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
server.serve_forever()
