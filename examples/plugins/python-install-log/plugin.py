"""Example external Wings plugin in Python (standard library only).

Wings starts it as its own process and sends it JSON-RPC 2.0 messages, one per
line, on stdin (see plugin/sdk/SPEC.md). It logs every server install that
starts or finishes, to stderr (which Wings logs) and to installs.log in the
plugin's data directory ($WINGS_PLUGIN_DATA).
"""
import json
import os
import sys
import time

DATA = os.environ.get("WINGS_PLUGIN_DATA", ".")


def reply(msg_id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg_id, "result": result}) + "\n")
    sys.stdout.flush()


def on_event(event):
    if event.get("type") != "server.install":
        return
    status = (event.get("data") or {}).get("status", "?")
    line = f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())} {event.get('server')} install {status}"
    print(line, file=sys.stderr, flush=True)
    with open(os.path.join(DATA, "installs.log"), "a", encoding="utf-8") as f:
        f.write(line + "\n")


for raw in sys.stdin:  # ends when Wings closes stdin
    try:
        msg = json.loads(raw)
    except ValueError:
        continue  # ignore a line that isn't JSON
    if not isinstance(msg, dict):
        continue
    method = msg.get("method")
    if method == "initialize":
        reply(msg.get("id"), {"name": "install-log", "capability": "events",
                              "provides": {"events": ["server.install"]}})
    elif method == "event" and isinstance(msg.get("params"), dict):
        on_event(msg["params"])
    elif msg.get("id") is not None:  # an unknown request needs an error reply
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg["id"],
                                     "error": {"code": -32601, "message": "method not found"}}) + "\n")
        sys.stdout.flush()
