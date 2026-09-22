#!/usr/bin/env python3
"""Test default-off and live model override using only synthetic loopback traffic."""
import json
import pathlib
import re
import signal
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import URLError
from urllib.request import Request, ProxyHandler, build_opener

ROOT = pathlib.Path(__file__).resolve().parents[1]
VALIDATION = ROOT / '.local' / 'validation'
CALLS = []
BODY = b'{"model":"fixture-original","input":{"model":"fixture-nested"},"stream":true}'
OPENER = build_opener(ProxyHandler({}))

class Upstream(BaseHTTPRequestHandler):
    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert data['input']['model'] == 'fixture-nested'
        CALLS.append(data['model'])
        payload = ('data: '+json.dumps({'type':'response.completed', 'response':{'model':data['model']}})+'\n\n').encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def log_message(self, *_):
        pass

def call(base, path, data=None, token=None):
    headers = {'Content-Type':'application/json'}
    if token:
        headers['X-Recorder-Token'] = token
    with OPENER.open(Request(base+path, data=data, headers=headers), timeout=5) as response:
        return response.headers, response.read()

def main():
    VALIDATION.mkdir(parents=True, exist_ok=True)
    up = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=up.serve_forever, daemon=True).start()
    proc = None
    try:
        with tempfile.TemporaryDirectory(prefix='model-switch-', dir=VALIDATION) as td:
            directory = pathlib.Path(td)
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            base = f'http://127.0.0.1:{port}'
            path = directory/'config.json'
            config = {'listen':f'127.0.0.1:{port}', 'upstream':f'http://127.0.0.1:{up.server_port}', 'directory':str(directory/'records'), 'mode':'redacted', 'force_model_enabled':False, 'force_model':''}
            path.write_text(json.dumps(config))
            log = directory/'process.log'
            with log.open('w') as output:
                proc = subprocess.Popen([str(ROOT/'bin/ccodex-request-recorder'), '-config', str(path)], stdout=output, stderr=output, cwd=ROOT)
                deadline = time.monotonic()+10
                while True:
                    if proc.poll() is not None:
                        raise RuntimeError('recorder stopped before readiness')
                    try:
                        call(base, '/healthz')
                        break
                    except (URLError, OSError):
                        if time.monotonic()>deadline:
                            raise RuntimeError('recorder readiness timeout')
                        time.sleep(.05)
                assert not CALLS, 'startup sent an upstream request'
                token = re.search(r'#token=([^\s]+)', log.read_text()).group(1)
                _, html = call(base, '/__recorder/')
                assert b'id="force-enabled"' in html and b'alias_equivalent' not in html
                _, status = call(base, '/__recorder/api/status', token=token)
                assert not json.loads(status)['model_override']['enabled']
                records = []
                for index, expected in enumerate(['fixture-original', 'fixture-forced', 'fixture-original']):
                    if index:
                        policy = {'enabled':index==1, 'model':'fixture-forced' if index==1 else ''}
                        _, answer = call(base, '/__recorder/api/model-override', json.dumps(policy).encode(), token)
                        assert json.loads(answer)['persisted'] is False
                    headers, _ = call(base, '/backend-api/codex/responses', BODY)
                    record_id = headers['X-Recorder-Request-Id']
                    deadline = time.monotonic()+5
                    while True:
                        try:
                            _, saved = call(base, '/__recorder/api/records/'+record_id, token=token)
                            record = json.loads(saved)
                            break
                        except URLError:
                            if time.monotonic()>deadline:
                                raise RuntimeError('record save timeout')
                            time.sleep(.05)
                    model = record['model_detection']
                    assert model['requested_model']=='fixture-original'
                    assert model['forwarded_model']==expected
                    assert model['upstream_declared_models']==[expected]
                    assert model['verdict']=='consistent'
                    assert model['request_model_rewritten']==(index==1)
                    assert not model['actual_execution_verified']
                    records.append(record)
                assert CALLS==['fixture-original','fixture-forced','fixture-original'], 'unexpected send or replay'
                assert json.loads(path.read_text())['force_model_enabled'] is False
                record_path = VALIDATION/'model-override-smoke-record.json'
                record_path.write_text(json.dumps(records[1], indent=2))
                record_path.chmod(0o600)
                analysis = subprocess.run([str(ROOT/'bin/ccodex-request-recorder'), '-analyze', str(record_path)], check=True, capture_output=True, text=True, timeout=5)
                model = json.loads(analysis.stdout)
                assert model['requested_model']=='fixture-original' and model['forwarded_model']=='fixture-forced' and model['verdict']=='consistent'
                proc.send_signal(signal.SIGTERM)
                assert proc.wait(timeout=10)==0
                result = {'default_off':True, 'live_enable_disable':True, 'original_outgoing_returned_separate':True, 'nested_model_unchanged':True, 'configuration_not_overwritten':True, 'offline_analysis':'passed', 'synthetic_upstream_calls':len(CALLS), 'real_upstream_calls':0, 'server_left_running':False}
                (VALIDATION/'model-override-smoke.json').write_text(json.dumps(result, indent=2))
                print(json.dumps(result, indent=2))
    finally:
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        up.shutdown()
        up.server_close()

if __name__ == '__main__':
    main()
