"use strict";
const $=id=>document.getElementById(id);
const labels={consistent:"标识一致",mismatch:"标识不同",conflict:"证据冲突",unknown:"未知",observed:"已观测 / 请求模型未知"};
const storageKey="ccodex-recorder-token";
let token=new URLSearchParams(location.hash.slice(1)).get("token")||sessionStorage.getItem(storageKey)||"";
if(location.hash)history.replaceState(null,"",location.pathname);
let rows=[],selected=null,recording=false,busy=false,forceDirty=false,forceRevision=0;
function node(tag,text,cls){const n=document.createElement(tag);n.textContent=text;if(cls)n.className=cls;return n;}
async function api(path,body){
 const r=await fetch("/__recorder/api/"+path,{method:body?"POST":"GET",headers:{"X-Recorder-Token":token,...(body?{"Content-Type":"application/json"}:{})},...(body?{body:JSON.stringify(body)}:{})});
 if(!r.ok)throw new Error(r.status===403?"令牌无效。请使用当前进程启动时输出的面板地址。":"操作失败，HTTP "+r.status);
 return r.json();
}
async function guarded(fn){if(busy)return;busy=true;$("error").textContent="";try{await fn();}catch(e){$("error").textContent=e.message;}finally{busy=false;}}
async function refresh(){
 const [s,list,groups]=await Promise.all([api("status"),api("records?limit=200"),api("observations")]);renderGroups(groups);renderOverride(s.model_override);rows=list;recording=s.recording;
 sessionStorage.setItem(storageKey,token);$("login").hidden=true;
 $("state").textContent=(recording?"记录中":"已暂停记录")+" · "+s.mode;
 $("toggle").disabled=false;$("toggle").textContent=recording?"暂停记录（继续转发）":"开始记录";
 $("stats").textContent=`${s.records} 条 · 进行中 ${s.inflight} · 队列 ${s.queue_depth} · 丢弃 ${s.dropped}（队列 ${s.queue_drops} / 配额 ${s.quota_drops} / 写盘 ${s.write_errors}） · ${(s.disk_bytes/1048576).toFixed(1)} MiB`;
 renderRows();
}
function renderRows(){
 $("records").replaceChildren();const q=$("filter").value.toLowerCase();
 for(const r of rows.filter(x=>JSON.stringify(x).toLowerCase().includes(q)&&(!$("verdict").value||x.model_verdict===$("verdict").value))){
  const b=node("button","","record");b.append(node("strong",labels[r.model_verdict]||r.model_verdict,"verdict "+r.model_verdict),node("span",`${r.requested_model||"原始模型未知"} → ${r.forwarded_model||"出站模型未知"} → ${(r.declared_models||[]).join(" / ")||"未返回模型证据"}`),node("small",`${r.status} · ${r.method} ${r.uri} · ${r.duration_ms} ms`),node("small",`${new Date(r.started_at).toLocaleString()} · ${r.outcome}${r.evidence_limited?" · 观测受限":""}`));
  b.append(node("small",quotaText(r.quota_observations)),node("small",`出站模型 ${r.forwarded_model||"未知"} · 响应头 ${r.response_headers_ms} ms · ${r.configured_route_label||"未标注路线"}`));
  b.addEventListener("click",()=>guarded(async()=>{selected=await api("records/"+r.id);renderDetail();}));$("records").append(b);
 }
 if(!$("records").children.length)$("records").append(node("p","暂无匹配记录。记录只在请求结束后保存。"));
}
function section(title,value,open=false){const d=document.createElement("details");d.open=open;d.append(node("summary",title),node("pre",typeof value==="string"?value:JSON.stringify(value,null,2)));return d;}
function renderDetail(){
 const r=selected;$("selected").textContent=`${r.id} · ${r.mode} · ${r.outcome}`;$("export").disabled=false;$("details").replaceChildren();
 const m=r.model_detection;
 $("details").append(section("强制模型改写决策（请求开始时的快照）",r.model_override||{applied:false},Boolean(r.model_override?.applied)));
 $("details").append(section("额度窗口观测（不是实时余额）",r.quota_observations||[],true),section("state / Cookie / 凭据作用域指纹",r.protocol_context));
 $("details").append(section("模型证据 · "+(labels[m.verdict]||m.verdict),m,true));
 $("details").append(section("捕获完整性",{request:{bytes:r.request_body.observed_bytes,captured:r.request_body.captured_bytes,truncated:r.request_body.truncated,eof:r.request_body.observed_eof,note:r.request_body.analysis_note},response:{bytes:r.response_body.observed_bytes,captured:r.response_body.captured_bytes,truncated:r.response_body.truncated,eof:r.response_body.observed_eof,note:r.response_body.analysis_note,events_limited:r.response_body.events_limited,timing_limited:r.response_body.timing_limited}},true));
 $("details").append(section("入站 / 出站请求头",{incoming:r.incoming_headers,outgoing:r.outgoing_headers,uri:r.uri,outgoing_uri:r.outgoing_uri,upstream:r.upstream_origin}));
 if(r.incoming_request_body)$("details").append(section("客户端原始请求正文（改写前）",r.incoming_request_body));
 $("details").append(section("实际出站请求正文",r.request_body));
 $("details").append(section("上游响应头",r.upstream_response_headers));
 $("details").append(section("SSE 事件时间线",r.response_body.events||[]));
 $("details").append(section("响应正文 / 原始捕获",r.response_body));
}
$("connect").addEventListener("click",()=>guarded(async()=>{token=$("token").value.trim();await refresh();$("token").value="";}));
$("refresh").addEventListener("click",()=>guarded(refresh));$("filter").addEventListener("input",renderRows);
$("toggle").addEventListener("click",()=>guarded(async()=>{await api("capture",{enabled:!recording});await refresh();}));
$("export").addEventListener("click",()=>{if(!selected)return;if(!confirm("记录可能包含提示词、回复和隐私信息；full 模式还可能含登录凭据。确认导出到本机？"))return;const u=URL.createObjectURL(new Blob([JSON.stringify(selected,null,2)],{type:"application/json"}));const a=document.createElement("a");a.href=u;a.download=selected.id+".json";a.click();setTimeout(()=>URL.revokeObjectURL(u),1000);});
if(token)guarded(refresh);
setInterval(()=>{if(token&&!document.hidden&&!busy)guarded(refresh);},3000);

function quotaText(windows){
 const latest=new Map();for(const w of windows||[])latest.set(w.limit_id+"/"+w.window,w);
 if(!latest.size)return "未观测到额度窗口";
 return [...latest.values()].map(w=>`${w.limit_id}/${w.window}: ${w.used_percent==null?"使用率未知":w.used_percent+"%"} · ${w.window_minutes==null?"时长未知":w.window_minutes+" 分钟"}${w.invalid_fields?.length?" · 字段异常":""}`).join("；");
}
function renderGroups(groups){
 const box=$("groups");box.replaceChildren();
 for(const g of groups.slice(0,100)){
  const scope=g.credential_scope_fingerprint||"旧记录未分组";
  box.append(node("p",`${g.configured_route_label||"未标注路线"} · ${g.upstream_origin} · ${g.requested_model||"原始模型未知"} → ${g.forwarded_model||"出站未知"} · ${scope.slice(0,12)} | ${g.samples} 条：一致 ${g.consistent}，不同 ${g.different}，冲突 ${g.conflict}，未知 ${g.unknown}；受限 ${g.limited}`));
 }
 if(!groups.length)box.append(node("p","暂无已保存观测。这里只统计记录，不据此更换代理或重发请求。"));
 if(groups.length>100)box.append(node("p",`共 ${groups.length} 组，页面只展开最近 100 组。API 返回完整分组。`));
}
$("verdict").addEventListener("change",renderRows);

function renderOverride(policy){
 if(!policy)return;
 for(const id of ["force-enabled","force-model","save-force"])$(id).disabled=false;
 if(!forceDirty){$("force-enabled").checked=policy.enabled;$("force-model").value=policy.model||"";}
 $("force-status").textContent=(policy.enabled?`已开启：生成请求将发往模型 ${policy.model}`:"已关闭：不会改写请求模型")+` · 策略版本 ${policy.revision} · 仅影响之后接入的请求`;
}
$("force-form").addEventListener("input",()=>{forceDirty=true;forceRevision++;});
$("force-form").addEventListener("submit",event=>{event.preventDefault();guarded(async()=>{
 const body={enabled:$("force-enabled").checked,model:$("force-model").value.trim()};
 if(body.enabled&&!body.model)throw new Error("开启前请填写目标模型。");
 if(body.enabled&&!confirm(`生成请求将把顶层 model 改为 ${body.model}。不换 state 或 Cookie，不重发请求，也不保证上游执行身份。仅本次进程生效，确认开启？`))return;
 const revision=forceRevision;
 await api("model-override",body);
 if(revision===forceRevision)forceDirty=false;
 await refresh();
});});
