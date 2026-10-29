'use strict';

// ---------- 常量 ----------
const RISK_NAME = ['只读', '写本地', '写外部', '不可逆'];
const KIND_LABEL = { plan: '执行计划确认', tool: '工具执行审批', stuck: '任务卡住，人工介入' };

const $ = id => document.getElementById(id);

// ---------- 状态 ----------
let ws = null;
let connected = false;
let authenticated = false;   // 是否已通过 Token 登录
let currentSessionID = null; // 当前选中的分支会话
let selectedMsgID = 0;       // 聊天区选中的可回滚消息
let pending = null;          // 待应答的审批 {requestID, kind}
let convCache = [];          // 会话树缓存

// ---------- HTTP 基础 ----------
async function api(path, method = 'GET', body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  if (r.status === 401) showLogin();
  if (!r.ok) {
    let msg = 'HTTP ' + r.status;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) {}
    throw new Error(msg);
  }
  if (r.status === 204) return null;
  return r.json();
}

// ---------- 登录 ----------
function setAuthenticated(on) {
  authenticated = on;
  $('btnLogout').classList.toggle('hidden', !on);
  if (on) {
    $('loginModal').classList.add('hidden');
    return;
  }
  // 未登录时断开连接，避免 WS 反复重连给服务端造成无效握手
  if (ws) {
    ws.onclose = null;
    ws.close();
    ws = null;
  }
  pending = null;
  $('approvalModal').classList.add('hidden');
  setStatus(false);
  $('loginModal').classList.remove('hidden');
  setTimeout(() => { if ($('loginToken')) $('loginToken').focus(); }, 0);
}

function showLogin() {
  if (!authenticated) {
    $('loginModal').classList.remove('hidden');
    return;
  }
  setAuthenticated(false);
}

async function checkAuth() {
  try {
    const r = await fetch('/api/auth/status', { credentials: 'same-origin' });
    if (r.ok) {
      const j = await r.json();
      if (j.authenticated) {
        setAuthenticated(true);
        startApp();
        return;
      }
    }
  } catch (_) {}
  setAuthenticated(false);
}

async function login() {
  const token = $('loginToken').value.trim();
  if (!token) return;
  try {
    const r = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify({ token }),
    });
    if (r.status === 204) {
      $('loginToken').value = '';
      $('loginErr').classList.add('hidden');
      setAuthenticated(true);
      startApp();
      return;
    }
    let msg = '登录失败';
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) {}
    $('loginErr').textContent = msg;
    $('loginErr').classList.remove('hidden');
  } catch (e) {
    $('loginErr').textContent = e.message;
    $('loginErr').classList.remove('hidden');
  }
}

async function logout() {
  try { await api('/api/auth/logout', 'POST'); } catch (_) {}
  setAuthenticated(false);
}

// ---------- WebSocket ----------
function connectWS() {
  if (!authenticated) return;
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onopen = () => setStatus(true);
  ws.onclose = () => {
    setStatus(false);
    if (authenticated) setTimeout(connectWS, 2000);
  };
  ws.onerror = () => {};
  ws.onmessage = e => { let m; try { m = JSON.parse(e.data); } catch (_) { return; } handleWS(m); };
}
function wsSend(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
  else toast('未连接到服务器', true);
}
function setStatus(on) {
  connected = on;
  const el = $('connStatus');
  el.textContent = on ? '已连接' : '已断开';
  el.className = 'status ' + (on ? 'online' : 'offline');
}

function handleWS(m) {
  switch (m.type) {
    case 'status': toast(m.message); break;
    case 'error': toast(m.message, true); break;
    case 'approval_request': showApproval(m.requestID, m.req); break;
    case 'event': onEvent(m.event); break;
    case 'final':
      toast('完成');
      refreshAfterRun();
      break;
  }
}

// ---------- 会话 / 分支 ----------
async function loadConversations() {
  try {
    convCache = await api('/api/conversations');
  } catch (e) {
    convCache = [];
  }
  renderConversations();
  if (!currentSessionID && convCache.length) {
    const first = convCache[0].Branches && convCache[0].Branches[0];
    if (first) selectBranch(first.SessionID).catch(() => {});
  }
}

function renderConversations() {
  const box = $('convList');
  box.innerHTML = '';
  $('convCount').textContent = String(convCache.length);
  if (!convCache.length) {
    box.innerHTML = '<div class="sb-empty">还没有会话，点击上方新建</div>';
    return;
  }
  // 按最近活跃时间排序，最新在最上
  const sorted = [...convCache].sort((a, b) => updatedOf(b) - updatedOf(a));
  for (const conv of sorted) {
    const activeBranches = conv.Branches || [];
    if (!activeBranches.length) continue;
    for (const b of activeBranches) {
      const el = document.createElement('div');
      el.className = 'sb-item' + (b.SessionID === currentSessionID ? ' active' : '');
      const label = b.Label || b.SessionID.slice(0, 12);
      const prev = (b.Preview || '').slice(0, 26) || '（空）';
      el.innerHTML = `
        <div class="t"><span class="dot"></span>${esc(label)}</div>
        <div class="d">${b.SessionID.slice(-8)} · ${b.MessageCount} 条消息</div>
        <div class="time">${esc(prettyTime(b.UpdatedAt ? new Date(b.UpdatedAt * 1000 || b.UpdatedAt) : null))} · ${esc(prev)}</div>`;
      el.onclick = () => selectBranch(b.SessionID);
      box.appendChild(el);
    }
  }
}

// updatedOf 提取会话树的最近活跃时间戳
function updatedOf(conv) {
  const bs = conv.Branches || [];
  let mx = 0;
  for (const b of bs) { const t = Number(b.UpdatedAt) || 0; if (t > mx) mx = t; }
  return mx;
}

// prettyTime 把时间戳格式化为「刚刚 / n 分钟前 / 今天 10:30」这类友好表示
function prettyTime(d) {
  if (!d || isNaN(d)) return '';
  const diff = Date.now() - d.getTime();
  const m = Math.floor(diff / 60000);
  if (m < 1) return '刚刚';
  if (m < 60) return m + ' 分钟前';
  const h = Math.floor(m / 60);
  if (h < 24) {
    const pad = n => String(n).padStart(2, '0');
    return '今天 ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }
  const dd = Math.floor(h / 24);
  if (dd === 1) return '昨天';
  if (dd < 30) return dd + ' 天前';
  return d.getFullYear() + '/' + (d.getMonth() + 1) + '/' + d.getDate();
}

async function selectBranch(id) {
  currentSessionID = id;
  selectedMsgID = 0;
  $('currentConvId').textContent = id;
  renderConversations();
  await loadBranches();
  await loadHistory();
}

async function loadBranches() {
  const box = $('branchList');
  box.innerHTML = '<div class="sb-empty">加载中…</div>';
  $('branchCount').textContent = '0';
  if (!currentSessionID) { box.innerHTML = '<div class="sb-empty">请选择会话</div>'; return; }
  try {
    const branches = await api(`/api/sessions/${enc(currentSessionID)}/branches`);
    box.innerHTML = '';
    $('branchCount').textContent = String(branches.length);
    for (const b of branches) {
      const el = document.createElement('div');
      el.className = 'sb-item' + (b.SessionID === currentSessionID ? ' active' : '');
      const label = b.Label || b.SessionID.slice(0, 12);
      el.innerHTML = `<div class="t"><span class="dot"></span>${esc(label)}</div>
        <div class="d">${b.SessionID.slice(-8)} · ${b.MessageCount} 条</div>`;
      el.onclick = () => selectBranch(b.SessionID);
      box.appendChild(el);
    }
  } catch (e) { box.innerHTML = '<div class="sb-empty">' + esc(e.message) + '</div>'; }
}

async function loadHistory() {
  const box = $('chat');
  box.innerHTML = '';
  if (!currentSessionID) { renderWelcome(box, '请选择或新建一个会话，开始你的任务。'); return; }
  try {
    const msgs = await api(`/api/sessions/${enc(currentSessionID)}`);
    if (!msgs.length) { renderWelcome(box, '该分支还没有消息，输入问题开始对话。'); return; }
    for (const m of msgs) addChatMsg(roleOf(m), m.Content, m.Kind, m.ID);
  } catch (e) {
    toast(e.message, true);
  }
}

// renderWelcome 空会话时的居中欢迎态：大标题 + 场景标签 + 快捷卡片
function renderWelcome(box, hint) {
  box.innerHTML = `
    <div class="welcome">
      <div class="brand">Simple<span class="g">Agent</span></div>
      <h1>我帮你完成任务</h1>
      <div class="tagline">${esc(hint)}</div>
      <div class="scene-tags">
        <button class="chip-pill" data-p="计算今天加上 365 天是哪一天，并给出建议">🔥 日常办公</button>
        <button class="chip-pill" data-p="列出工作区当前目录结构，并说明主要模块">💻 代码开发</button>
        <button class="chip-pill" data-p="搜索工作区中与上下文压缩相关的代码并总结">🔍 深度查询</button>
      </div>
      <div class="welcome-quick">
        <div class="scene-card" data-p="列出工作区根目录的内容">
          <span class="card-ico">📁</span><span class="card-txt">查看工作区</span>
        </div>
        <div class="scene-card" data-p="搜索项目中所有与 AgentEvent 相关的代码">
          <span class="card-ico">🔎</span><span class="card-txt">搜索代码</span>
        </div>
        <div class="scene-card" data-p="读取 agent/agent.go 的前 60 行并总结结构">
          <span class="card-ico">📄</span><span class="card-txt">读取文件</span>
        </div>
        <div class="scene-card" data-p="列出当前已注册的所有工具和技能，并说明各自用途">
          <span class="card-ico">🧰</span><span class="card-txt">查看工具</span>
        </div>
      </div>
    </div>`;
  box.querySelectorAll('[data-p]').forEach(el => {
    el.onclick = () => { $('input').value = el.dataset.p; $('input').focus(); };
  });
}

function roleOf(m) {
  if (m.Role === 'user') return m.Kind === 'guidance' ? 'guidance' : 'user';
  return 'assistant';
}

function addChatMsg(role, text, kind, id) {
  const box = $('chat');
  // 移除欢迎态，切换到消息列表布局
  box.querySelector('.welcome')?.remove();
  const el = document.createElement('div');
  el.className = 'msg ' + (role === 'tool' ? 'tool' : role);
  if (kind === 'tool_result' || kind === 'tool_call' || role === 'tool') el.classList.add('tool');
  // 这里 text 可能包含 <tool_output> 边界，作为纯文本展示
  el.textContent = text;
  if (id && role === 'user' && kind !== 'guidance') {
    // 用户提问可作为回滚目标：点击选中
    el.classList.add('rollable');
    el.style.cursor = 'pointer';
    el.onclick = () => {
      selectedMsgID = id;
      box.querySelectorAll('.msg.selected').forEach(x => x.classList.remove('selected'));
      el.classList.add('selected');
      $('branchLabel').textContent = '已选可回滚消息 #' + id;
    };
  }
  box.appendChild(el);
  box.scrollTop = box.scrollHeight;
}

// ---------- 事件日志 ----------
function onEvent(ev) {
  const list = $('eventList');
  const el = document.createElement('div');
  el.className = 'ev';
  let head, extra = '';
  switch (ev.Type) {
    case 'round': head = `🧠 ${ev.Message}`; break;
    case 'plan_ready': head = '📋 计划就绪'; extra = ev.Message; break;
    case 'tool_call': head = `🔧 调用 ${ev.ToolName}`; extra = ev.Args; break;
    case 'tool_result': head = `✅ ${ev.ToolName} 结果`; extra = String(ev.Result || '').slice(0, 1500); break;
    case 'tool_rejected': head = `🚫 ${ev.ToolName} 被拒绝`; extra = ev.Message; break;
    case 'stuck': head = '⚠️ 卡住转人工'; extra = ev.Message; break;
    case 'final': head = '🏁 得到最终答案'; break;
    default: head = ev.Type; break;
  }
  el.innerHTML = `<b>${esc(head)}</b>`;
  if (extra) { const d = document.createElement('div'); d.className = 'r'; d.textContent = extra; el.appendChild(d); }
  list.prepend(el);
}

// ---------- 审批 ----------
function showApproval(requestID, req) {
  pending = { requestID, kind: req.Kind };
  $('apprTitle').textContent = KIND_LABEL[req.Kind] || '需要人工确认';

  let html = `<div class="kv">类型：<b>${esc(KIND_LABEL[req.Kind] || req.Kind)}</b></div>`;
  if (req.Risk !== undefined) html += `<div class="kv">风险：<b>${RISK_NAME[req.Risk] || req.Risk}</b></div>`;
  if (req.Round) html += `<div class="kv">轮次：第 <b>${req.Round}</b> 轮</div>`;
  if (req.ToolName) html += `<div class="kv">工具：<b>${esc(req.ToolName)}</b></div>`;
  if (req.Summary) html += `<div class="kv">说明：<b>${esc(req.Summary)}</b></div>`;
  if (req.Arguments) html += `<div class="kv" style="margin-bottom:4px">内容：</div><div class="req-body">${esc(req.Arguments)}</div>`;

  $('apprBody').innerHTML = html;

  const text = $('apprText');
  text.value = req.Arguments || '';
  $('apprReason').value = '';

  // 定制按钮
  const isStuck = req.Kind === 'stuck';
  $('apprApprove').textContent = isStuck ? '收工' : '放行';
  $('apprApprove').className = isStuck ? 'btn danger' : 'btn primary';
  $('apprReject').textContent = isStuck ? '收工' : '拒绝';
  $('apprModify').textContent = isStuck ? '给新指令' : '修改后执行';

  // 修改/新指令场景使用文本框，统一展示
  $('apprText').classList.remove('hidden');
  $('apprModify').onclick = () => {
    const args = text.value;
    if (!args) { toast('内容不能为空', true); return; }
    respond(requestID, 'modify', args, args);
    closeApproval();
  };
  $('apprApprove').onclick = () => {
    if (isStuck) respond(requestID, 'reject', '收工', '');
    else respond(requestID, 'approve', '', '');
    closeApproval();
  };
  $('apprReject').onclick = () => {
    const reason = $('apprReason').value;
    if (isStuck) respond(requestID, 'reject', reason || '收工', '');
    else respond(requestID, 'reject', reason, reason);
    closeApproval();
  };

  $('approvalModal').classList.remove('hidden');
}

function respond(requestID, decision, reason, arguments_) {
  wsSend({ type: 'approval', requestID, decision, reason, arguments: arguments_ });
}
function closeApproval() { $('approvalModal').classList.add('hidden'); pending = null; }

// ---------- 工具 / 技能 ----------
async function loadTools() {
  try {
    const tools = await api('/api/tools');
    const box = $('toolList');
    box.innerHTML = '';
    for (const t of tools) {
      const risk = RISK_NAME[t.Risk] || t.Risk;
      const auto = t.AutoApprove ? '自动' : '需确认';
      const el = document.createElement('div');
      el.className = 'sb-item';
      el.innerHTML = `<div class="t">${esc(t.Name)}</div><div class="d">${esc(risk)} · ${esc(auto)}</div>`;
      box.appendChild(el);
    }
  } catch (e) { /* 忽略 */ }
}

async function loadSkills() {
  const box = $('skillsList');
  box.innerHTML = '<div class="sb-empty">加载中…</div>';
  try {
    const skills = await api('/api/skills');
    box.innerHTML = '';
    if (!skills.length) box.innerHTML = '<div class="sb-empty">还没有自定义技能</div>';
    for (const s of skills) {
      const el = document.createElement('div');
      el.className = 'skill-item';
      el.innerHTML = `<div><div class="t">${esc(s.name)}</div><div class="d">${esc(s.description || '')}</div></div>
        <button class="chip" data-name="${esc(s.name)}">删除</button>`;
      el.querySelector('button').onclick = async () => {
        try { await api('/api/skills/' + enc(s.name), 'DELETE'); toast('已删除 ' + s.name); await loadSkills(); await loadTools(); }
        catch (e) { toast(e.message, true); }
      };
      box.appendChild(el);
    }
  } catch (e) { box.innerHTML = '<div class="sb-empty">' + esc(e.message) + '</div>'; }
}

// ---------- 对话 ----------
function sendChat() {
  const input = $('input');
  const text = input.value.trim();
  if (!text) return;
  if (!connected) return toast('未连接服务器', true);
  if (!currentSessionID) return toast('请先新建或选择一个会话', true);
  input.value = '';
  autoResize();
  addChatMsg('user', text);
  wsSend({ type: 'chat', sessionID: currentSessionID, message: text });
}

// autoResize textarea 随内容自适应增高，避免固定输入框高度
function autoResize() {
  const el = $('input');
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 200) + 'px';
}

// ---------- 回滚 / 分叉 / 新建 ----------
async function doRollback() {
  if (!currentSessionID) return toast('请先选择会话', true);
  if (!selectedMsgID) return toast('请先点击一条你自己的提问作为回滚点', true);
  try {
    const r = await api(`/api/sessions/${enc(currentSessionID)}/rollback`, 'POST', { index: selectedMsgID });
    toast('已回滚，原路径存档为 ' + (r.archiveID || ''));
    selectedMsgID = 0;
    await loadBranches(); await loadHistory();
  } catch (e) { toast(e.message, true); }
}

async function doFork() {
  if (!currentSessionID) return toast('请先选择会话', true);
  let body = {};
  if (selectedMsgID) body = { index: selectedMsgID };
  try {
    const r = await api(`/api/sessions/${enc(currentSessionID)}/fork`, 'POST', body);
    toast('已分叉出新分支');
    selectedMsgID = 0;
    await selectBranch(r.sessionID);
  } catch (e) { toast(e.message, true); }
}

async function newConversation() {
  try {
    const r = await api('/api/conversations', 'POST');
    await loadConversations();
    await selectBranch(r.sessionID);
  } catch (e) { toast(e.message, true); }
}

// ---------- 刷新 ----------
async function refreshAfterRun() {
  if (!currentSessionID) return;
  await loadConversations();
  await loadBranches();
  await loadHistory();
}

// ---------- 工具函数 ----------
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function enc(s) { return encodeURIComponent(s); }
let toastTimer = null;
function toast(msg, isErr) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast' + (isErr ? ' error' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add('hidden'), 3200);
}

// ---------- 事件绑定 ----------
function bind() {
  $('btnSend').onclick = sendChat;
  const inputEl = $('input');
  inputEl.addEventListener('input', autoResize);
  inputEl.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendChat();
    }
  });
  $('btnStop').onclick = () => wsSend({ type: 'stop' });
  $('btnNewConv').onclick = newConversation;
  $('btnRollback').onclick = doRollback;
  $('btnFork').onclick = doFork;
  $('btnToggleEvents').onclick = () => $('eventPanel').classList.toggle('hidden');
  $('btnClearEvents').onclick = () => { $('eventList').innerHTML = ''; };
  $('btnAttach').onclick = () => toast('附件上传暂未启用');
  $('btnShowSkills').onclick = () => { loadSkills(); $('skillsModal').classList.remove('hidden'); };
  $('btnAddSkill').onclick = async () => {
    const name = $('skName').value.trim();
    const desc = $('skDesc').value.trim();
    const cmd = $('skCmd').value.trim();
    if (!name || !desc || !cmd) return toast('名称、描述、命令模板均必填', true);
    try {
      // 命令技能在服务端固定按最高风险处理，前端不再提交可伪造的风险级别
      await api('/api/skills', 'POST', { name, description: desc, command_template: cmd });
      toast('已新增技能 ' + name);
      $('skName').value = $('skDesc').value = $('skCmd').value = '';
      await loadSkills(); await loadTools();
    } catch (e) { toast(e.message, true); }
  };
  $('approvalModal').addEventListener('click', e => { if (e.target.id === 'approvalModal') closeApproval(); });
  $('skillsModal').addEventListener('click', e => { if (e.target.id === 'skillsModal') $('skillsModal').classList.add('hidden'); });
  $('btnLogin').addEventListener('click', login);
  $('loginToken').addEventListener('keydown', e => { if (e.key === 'Enter') login(); });
  $('btnLogout').addEventListener('click', logout);
}

// ---------- 启动 ----------
function startApp() {
  connectWS();
  loadTools();
  loadConversations();
  // 若启动时就已存在会话，loadConversations 内部会选中第一条；
  // 否则渲染欢迎态，让用户看到开场界面而不是空白
  if (!currentSessionID) loadHistory();
}

function init() {
  bind();
  // 先探测登录状态，未登录时只显示登录框，不发起业务请求
  checkAuth();
}
init();