#!/usr/bin/env python3
"""Exercise the compiled recorder against a synthetic loopback-only upstream."""
import json, pathlib, re, signal, socket, subprocess, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, build_opener, ProxyHandler
from urllib.error import HTTPError, URLError
ROOT = pathlib.Path(__file__).resolve().parents[1]
VALIDATION = ROOT / '.local' / 'validation'
VALIDATION.mkdir(parents=True, exist_ok=True)
CALLS = 0
BODY = b'data: {"type":"response.completed","response":{"model":"fixture-served"}}\n\n'
class Upstream(BaseHTTPRequestHandler):
    def do_POST(self):
        global CALLS
        CALLS += 1
        self.rfile.read(int(self.headers.get('Content-Length', '0')))
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(BODY)))
        self.send_header('X-Codex-Primary-Used-Percent', '63.5')
        self.send_header('X-Codex-Primary-Window-Minutes', '10080')
        self.end_headers()
        self.wfile.write(BODY)
    def log_message(self, *_):
        pass
opener = build_opener(ProxyHandler({}))
def call(base, path, data=None, token=None):
    headers = {'Content-Type': 'application/json'}
    if token: headers['X-Recorder-Token'] = token
    return opener.open(Request(base+path, data=data, headers=headers), timeout=3)
def main():
    up = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=up.serve_forever, daemon=True).start()
    proc = None
    try:
        with tempfile.TemporaryDirectory(prefix='recorder-smoke-', dir=VALIDATION) as td:
            directory = pathlib.Path(td)
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
            base = f'http://127.0.0.1:{port}'
            config = {'listen': f'127.0.0.1:{port}', 'upstream': f'http://127.0.0.1:{up.server_port}', 'directory': str(directory/'records'), 'mode': 'redacted', 'route_label': 'synthetic-loopback'}
            path = directory/'config.json'; path.write_text(json.dumps(config))
            log = directory/'process.log'
            with log.open('w') as output:
                proc = subprocess.Popen([str(ROOT/'bin/ccodex-request-recorder'), '-config', str(path)], stdout=output, stderr=output, cwd=ROOT)
                deadline = time.monotonic()+10
                while True:
                    if proc.poll() is not None: raise RuntimeError('recorder exited before readiness')
                    try:
                        with call(base, '/healthz') as response: json.load(response)
                        break
                    except (URLError, OSError):
                        if time.monotonic()>deadline: raise RuntimeError('startup timeout')
                        time.sleep(.05)
                assert CALLS == 0, 'startup contacted upstream'
                token = re.search(r'#token=([^\s]+)', log.read_text()).group(1)
                with call(base, '/__recorder/') as response:
                    assert b'id="verdict"' in response.read()
                try:
                    call(base, '/__recorder/api/records')
                    raise AssertionError('unauthorized records exposed')
                except HTTPError as exc:
                    assert exc.code == 403
                request = b'{"model":"fixture-requested","input":"synthetic smoke test","stream":true}'
                with call(base, '/backend-api/codex/responses', request) as response:
                    assert response.read() == BODY
                deadline = time.monotonic()+5
                while True:
                    with call(base, '/__recorder/api/records', token=token) as response: records=json.load(response)
                    if records: break
                    if time.monotonic()>deadline: raise RuntimeError('record save timeout')
                    time.sleep(.05)
                with call(base, '/__recorder/api/records/'+records[0]['id'], token=token) as response: record=json.load(response)
                assert record['model_detection']['verdict'] == 'mismatch'
                assert record['quota_observations'][0]['window_minutes'] == 10080
                assert not record['model_detection']['actual_execution_verified']
                with call(base, '/__recorder/api/observations', token=token) as response: groups=json.load(response)
                assert groups[0]['different'] == 1
                with call(base, '/__recorder/api/capture', b'{"enabled":false}', token) as response: json.load(response)
                with call(base, '/backend-api/codex/responses', request) as response: assert response.read() == BODY
                time.sleep(.1)
                with call(base, '/__recorder/api/records', token=token) as response: assert len(json.load(response)) == 1
                assert CALLS == 2, 'a request was replayed or missing'
                saved = VALIDATION/'recorder-smoke-record.json'
                saved.write_text(json.dumps(record, ensure_ascii=False, indent=2))
                saved.chmod(0o600)
                offline = subprocess.run([str(ROOT/'bin/ccodex-request-recorder'), '-analyze', str(saved)], capture_output=True, check=True, text=True, timeout=5)
                assert json.loads(offline.stdout)['verdict'] == 'mismatch'
                proc.send_signal(signal.SIGTERM)
                assert proc.wait(timeout=10) == 0
                result = {'health': 'passed', 'panel_served': True, 'unauthorized_records_denied': True, 'model_mismatch_detected': True, 'quota_window_minutes': 10080, 'pause_preserved_forwarding': True, 'offline_analysis': 'passed', 'synthetic_upstream_calls': CALLS, 'real_upstream_calls': 0, 'exit_code': proc.returncode, 'server_left_running': False}
                (VALIDATION/'recorder-smoke.json').write_text(json.dumps(result, indent=2))
                print(json.dumps(result, indent=2))
    finally:
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try: proc.wait(timeout=5)
            except subprocess.TimeoutExpired: proc.kill(); proc.wait(timeout=5)
        up.shutdown(); up.server_close()
if __name__ == '__main__':
    main()
