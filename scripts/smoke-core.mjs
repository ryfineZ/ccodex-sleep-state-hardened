// Browser test against the disposable loopback fixture created by smoke-core.py.
import fs from 'node:fs/promises';
const cfg=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
const pages=await (await fetch(`http://127.0.0.1:${cfg.debugPort}/json/list`)).json();
const page=pages.find(p=>p.type==='page');
if(!page)throw new Error('No isolated browser page');
const ws=new WebSocket(page.webSocketDebuggerUrl);await new Promise((resolve,reject)=>{ws.onopen=resolve;ws.onerror=reject;});
let id=0;const waiting=new Map();const exceptions=[];
ws.onmessage=event=>{const msg=JSON.parse(event.data);if(msg.method==='Page.javascriptDialogOpening'){send('Page.handleJavaScriptDialog',{accept:true}).catch(()=>{});}if(msg.method==='Runtime.exceptionThrown')exceptions.push(msg.params.exceptionDetails.text);if(msg.id){const p=waiting.get(msg.id);waiting.delete(msg.id);if(msg.error)p.reject(new Error(msg.error.message));else p.resolve(msg.result);}};
function send(method,params={}){const next=++id;return new Promise((resolve,reject)=>{waiting.set(next,{resolve,reject});ws.send(JSON.stringify({id:next,method,params}));});}
async function evaluate(expression){const result=await send('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});if(result.exceptionDetails)throw new Error(result.exceptionDetails.text);return result.result.value;}
async function until(expression){const end=Date.now()+12000;while(Date.now()<end){if(await evaluate(expression))return;await new Promise(r=>setTimeout(r,80));}throw new Error('Browser condition timed out: '+expression);}
try{
 await send('Runtime.enable');await send('Page.enable');await send('Emulation.setDeviceMetricsOverride',{width:1360,height:1050,deviceScaleFactor:1,mobile:false});
 await send('Page.navigate',{url:cfg.panel});
 await until('document.getElementById("quick-start") && !document.getElementById("quick-start").hidden && document.getElementById("login").hidden');
 if(await evaluate('location.hash'))throw new Error('Launch fragment was not removed');
 await evaluate('document.getElementById("proxy-mode").value="direct"; document.getElementById("setup-connect").click()');
 await until('document.getElementById("setup-title").textContent.includes("已接入")');
 await evaluate('document.getElementById("core-enabled").checked=true; document.getElementById("core-form").requestSubmit()');
 await until('document.getElementById("core-status").textContent.includes("已开启")');
 const response=await fetch(cfg.base+'/backend-api/codex/responses',{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer fixture-browser-core'},body:'{"model":"fixture-original-model","input":"synthetic browser smoke","stream":true}'});
 if(response.status!==200)throw new Error('Synthetic request failed: '+response.status);await response.text();
 await evaluate('document.getElementById("refresh").click()');
 await until('document.querySelectorAll("#records .record").length===1');
 await evaluate('document.querySelector("#records .record").click()');
 await until('document.getElementById("details").textContent.includes("fixture-original-model")');
 await until('document.getElementById("core-sessions").textContent.includes("组合可用")');
 if(!(await evaluate('document.getElementById("details").textContent.includes("State + Cookie 注入快照")')))throw new Error('Injection evidence missing');
 if(exceptions.length)throw new Error('Page exceptions: '+exceptions.join(';'));
 const image=await send('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});await fs.writeFile(cfg.screenshot,Buffer.from(image.data,'base64'));
 await evaluate('document.getElementById("setup-stop").click()');
 await until('document.getElementById("setup-title").textContent==="已退出"');
 console.log(JSON.stringify({core_enabled_from_panel:true,collected_state_and_cookies:true,injected_formal_request:true,automatic_login:true,one_click_connect:true,request_visible:true,detail_opens:true,restore_and_exit:true,page_exceptions:exceptions.length}));
}finally{ws.close();}
