#!/usr/bin/env python3
"""End-to-end one-click onboarding using only disposable Codex files and a local upstream.
An isolated headless Chrome profile is used when Chrome is installed. No real login,
proxy settings, system trust store or existing browser profile is read or changed.
"""
import base64, struct
import json, os, pathlib, re, signal, socket, subprocess, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, build_opener, ProxyHandler
from urllib.error import URLError
ROOT=pathlib.Path(__file__).resolve().parents[1]
OUT=ROOT/'.local/validation';OUT.mkdir(parents=True,exist_ok=True)
OPENER=build_opener(ProxyHandler({}))
CALLS=PROBES=GENERATIONS=0
RAW=bytearray(57+16*10);RAW[0]=128;RAW[1:9]=struct.pack(">Q",int(time.time()));TOKEN=base64.urlsafe_b64encode(RAW).decode()
class Upstream(BaseHTTPRequestHandler):
    def do_POST(self):
        global CALLS,PROBES,GENERATIONS
        CALLS+=1
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path!='/backend-api/codex/responses' or body.get('model')!='fixture-original-model':
            self.send_error(400);return
        is_probe=not self.headers.get('X-Codex-Turn-State')
        if is_probe:
            PROBES+=1
            if body.get('input')=='synthetic browser smoke':self.send_error(400);return
        else:
            GENERATIONS+=1
            cookie=self.headers.get('Cookie','')
            if self.headers.get('X-Codex-Turn-State')!=TOKEN or '__cflb=fixture' not in cookie or '__oailb=fixture' not in cookie:
                self.send_error(400);return
        data=b'data: {"type":"response.completed","response":{"model":"fixture-original-model"}}\n\n'
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('X-Codex-Turn-State',TOKEN)
        if is_probe:
            self.send_header('Set-Cookie','__cflb=fixture; Path=/')
            self.send_header('Set-Cookie','__oailb=fixture; Path=/')
        self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    def log_message(self,*_):pass

def port():
    with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def wait_url(url):
    deadline=time.monotonic()+12
    while time.monotonic()<deadline:
        try:
            with OPENER.open(url,timeout=1) as r:return json.load(r)
        except (URLError,OSError):time.sleep(.05)
    raise RuntimeError('Local service startup timed out')
def call(base,path,body,token=None):
    h={'Content-Type':'application/json'}
    if token:h['X-Recorder-Token']=token
    with OPENER.open(Request(base+path,data=json.dumps(body).encode(),headers=h),timeout=5) as r:return json.load(r)
def main():
    up=ThreadingHTTPServer(('127.0.0.1',0),Upstream);threading.Thread(target=up.serve_forever,daemon=True).start()
    proc=chrome=None
    try:
        with tempfile.TemporaryDirectory(prefix='oneclick-fixture-',dir=OUT) as temp:
            d=pathlib.Path(temp);home=d/'codex';home.mkdir();client=home/'config.toml'
            original=(f"# disposable fixture, never a real login\nmodel = 'fixture-original-model'\nmodel_provider = 'fixture'\n[model_providers.fixture]\nname = 'Fixture'\nbase_url = 'http://127.0.0.1:{up.server_port}/backend-api/codex'\nwire_api = 'responses'\nenv_key = 'UNUSED_FIXTURE_KEY'\nrequires_openai_auth = false\n").encode();client.write_bytes(original)
            listen=port();base=f'http://127.0.0.1:{listen}';config=d/'recorder.json';config.write_text(json.dumps({'listen':f'127.0.0.1:{listen}','upstream':'https://chatgpt.com','directory':str(d/'records'),'mode':'metadata'}))
            args=[str(ROOT/'bin/ccodex-request-recorder'),'setup','-no-browser','-config',str(config),'-codex-home',str(home)]
            log=d/'server.log'
            with log.open('w') as output:
                proc=subprocess.Popen(args,stdout=output,stderr=output,cwd=ROOT)
                wait_url(base+'/healthz')
                assert client.read_bytes()==original and CALLS==0,'startup modified client or called upstream'
                text=log.read_text();panel=re.search(r'http://127\.0\.0\.1:\d+/__recorder/#launch=[a-f0-9]+',text).group(0)
                reopened=subprocess.run(args,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=6,check=True)
                panel=re.search(r'http://127\.0\.0\.1:\d+/__recorder/#launch=[a-f0-9]+',reopened.stdout).group(0)
                chrome_path='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
                if pathlib.Path(chrome_path).exists():
                    debug=port()
                    with (d/'chrome.log').open('w') as chrome_out:
                        chrome=subprocess.Popen([chrome_path,'--headless','--no-first-run','--disable-background-networking','--disable-component-update',f'--user-data-dir={d/"browser-profile"}',f'--remote-debugging-port={debug}','--remote-debugging-address=127.0.0.1','about:blank'],stdout=chrome_out,stderr=chrome_out)
                        wait_url(f'http://127.0.0.1:{debug}/json/list')
                        job=d/'browser-job.json';job.write_text(json.dumps({'debugPort':debug,'panel':panel,'base':base,'screenshot':str(OUT/'core-panel.png')}))
                        browser=subprocess.run(['node',str(ROOT/'scripts/smoke-core.mjs'),str(job)],text=True,capture_output=True,timeout=45)
                        if browser.returncode:raise RuntimeError('Isolated browser test: '+browser.stderr)
                        report=json.loads(browser.stdout)
                else:
                    raise RuntimeError('Isolated Chrome is required for this browser acceptance test')
                    ticket=panel.split('#launch=')[1]
                    token=call(base,'/__recorder/api/launch',{'ticket':ticket})['token']
                    call(base,'/__recorder/api/setup/connect',{'confirm':True,'proxy_mode':'direct'},token)
                    assert b'fixture-original-model' in client.read_bytes()
                    call(base,'/__recorder/api/setup/stop',{'confirm':True},token)
                    report={'automatic_login':True,'one_click_connect':True,'restore_and_exit':True,'browser':'not installed; HTTP smoke only'}
                assert PROBES==1 and GENERATIONS==1, 'expected one probe and one generation'
                assert proc.wait(timeout=10)==0
                assert client.read_bytes()==original,'original config not restored exactly'
                report.update({'version':'0.5.0-state-cookie','startup_did_not_modify_codex':True,'second_launch_reopens':True,'original_config_restored_byte_exact':True,'synthetic_upstream_calls':CALLS,'synthetic_probes':PROBES,'synthetic_generations':GENERATIONS,'real_upstream_calls':0,'server_left_running':False})
                (OUT/'core-smoke.json').write_text(json.dumps(report,indent=2));print(json.dumps(report,indent=2))
    finally:
        for p in [chrome,proc]:
            if p is not None and p.poll() is None:
                p.terminate()
                try:p.wait(timeout=5)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
        up.shutdown();up.server_close()
if __name__=='__main__':main()
