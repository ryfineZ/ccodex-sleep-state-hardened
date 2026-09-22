"use strict";
let coreDirty=false,coreRevision=0,coreProxyDirty=false;
const corePhases={record_only:"仅记录",collecting:"正在采集",ready:"组合可用",waiting_for_bundle:"等待可用组合",waiting_for_request:"等待请求",account_paused:"账号已暂停"};
const coreMessages={accepted:"采集成功",injected_response_completed:"已注入，回复完整结束",state_core_probe_cooldown:"采集冷却中",state_core_probe_budget:"本小时采集预算已用完",state_core_shape_mismatch:"state 形状不符合账号规则",state_core_cookie_missing:"未收到可用路由 Cookie",state_core_state_time_rejected:"state 时间不符合规则",state_core_routes_cooling:"出口正在冷却",state_core_shape_suspect:"返回 state 形状异常",state_core_rate_limited:"上游要求暂停",state_core_auth_blocked:"登录或权限被拒绝",state_core_upstream_stream_error:"上游流中明确报告失败",state_core_probe_incomplete:"采集回复未完整结束",state_core_probe_network_failed:"采集出口连接失败",state_core_transport_failure:"正式回复传输失败",configuration_changed:"设置已更新，等待重新采集"};
function renderCore(s){
 if(!s)return;const p=s.policy;
 if(!coreDirty){$("core-enabled").checked=p.enabled;$("core-account").value=p.account_mode;$("core-fresh").value=p.freshness_seconds;$("core-cooldown").value=p.probe_cooldown_seconds;$("core-timeout").value=p.probe_timeout_seconds;$("core-attempts").value=p.max_probes_per_round;$("core-budget").value=p.probe_budget_per_hour;}
 $("core-status").textContent=p.enabled?`已开启：按需采集，在关联出口注入 state + Cookie。自定义出口 ${s.proxy_count} 个（0 表示使用当前连接）。` :"已关闭：仅转发/记录，不自动采集或注入。";
 const box=$("core-sessions");box.replaceChildren();
 for(const row of s.sessions||[]){
  const card=node("div","","core-session");
  card.append(node("strong",`${row.model} · ${corePhases[row.phase]||row.phase}`),node("p",`state ${row.state_length||0} 字符 · 票龄 ${row.state_age_seconds||0} 秒 · 本地新鲜度剩余 ${row.local_freshness_remaining||0} 秒 · Cookie ${row.cookie_count||0} 个 · 注入发送尝试 ${row.injections||0} 次`),node("p",`出口 ${row.route||"待采集"} · 冷却 ${row.cooldown_seconds||0} 秒 · 本小时采集 ${row.probes_this_hour||0} 次 · ${coreMessages[row.result]||row.result||"等待请求"}`));
  if(row.rejected_status)card.append(node("p",`上游 ${row.rejected_status}：切换模式、模型或出口不会重置账号暂停。`));
  const b=node("button","补充 / 刷新备用组合");b.disabled=!p.enabled||row.phase==="collecting"||row.phase==="account_paused"||(row.cooldown_seconds||0)>0;
  b.addEventListener("click",()=>guarded(async()=>{if(!confirm("将使用会话内存中的凭据发送有限短采集请求，可能消耗额度。仍遵守冷却和预算，并保留现有可用组合。继续？"))return;try{await api("state-core/refresh",{id:row.id,confirm_cost:true});}catch(e){throw new Error(coreMessages[e.message]||e.message);}finally{await refresh();}}));card.append(b);box.append(card);
 }
 if(!box.children.length)box.append(node("p",p.enabled?"等待 Codex 请求提供凭据，尚未发送采集请求。":"开启后，到 Codex 正常发消息即可自动采集。"));
 $("core-routes").replaceChildren(...(s.routes||[]).map(r=>node("p",`${r.id} · ${r.health.state} · 连续失败 ${r.health.consecutive_failures} · 累计网络故障 ${r.health.transport_failures}`)));
 $("core-probes").replaceChildren(...[...(s.recent_probes||[])].reverse().map(r=>node("p",`${new Date(r.at).toLocaleTimeString()} · ${r.model} · ${r.route} · HTTP ${r.http_status} · ${coreMessages[r.result]||r.result} · state ${r.state_length} / Cookie ${r.cookie_count}`)));
}
$("core-form").addEventListener("input",()=>{coreDirty=true;coreRevision++;});
$("core-proxies").addEventListener("input",()=>{coreProxyDirty=true;});
$("core-form").addEventListener("submit",event=>{
 event.preventDefault();
 guarded(async()=>{
  const enabled=$("core-enabled").checked;
  if(enabled&&!confirm("开启后会使用后续客户端请求的凭据发送有限短采集请求，可能消耗额度。实验性复用不保证有效。确认开启？"))return;
  const revision=coreRevision;
  const body={enabled,confirm_cost:enabled,account_mode:$("core-account").value,freshness_seconds:Number($("core-fresh").value),probe_cooldown_seconds:Number($("core-cooldown").value),probe_timeout_seconds:Number($("core-timeout").value),max_probes_per_round:Number($("core-attempts").value),probe_budget_per_hour:Number($("core-budget").value)};
  if(coreProxyDirty)body.proxy_urls=$("core-proxies").value.split(/\r?\n/).map(x=>x.trim()).filter(Boolean);
  await api("state-core/configure",body);
  if(revision===coreRevision){coreDirty=false;coreProxyDirty=false;}
  await refresh();
 });
});
