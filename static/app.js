/* 网络面板 —— 前端测速引擎与界面逻辑
 *
 * 核心：所有测速请求都走同源的 /api/proxy，这样 response.body 可读、
 * Content-Length 可见、也不会被混合内容策略拦掉。跨域问题在服务端解决。
 */

'use strict';

const $ = (id) => document.getElementById(id);

const state = {
  running: false,
  groups: [],
  sources: [],       // 拍平后的列表，方便按 index 取
  src: null,
  threads: 8,
  totalBytes: 0,
  startAt: 0,        // performance.now()
  lastBytes: 0,
  lastAt: 0,
  curSpeed: 0,
  avgSpeed: 0,
  samples: [],       // 最近 N 个采样点（字节/秒）
  aborts: [],
  ticker: null,
  keepBg: false,
};

const MAX_SAMPLES = 120;
const TICK_MS = 500;

/* ===== 工具 ===== */

function fmtBytes(n) {
  if (!isFinite(n) || n <= 0) return '-';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (n >= 100 ? n.toFixed(0) : n.toFixed(2)) + ' ' + u[i];
}

function fmtSpeed(bps) {
  if (!isFinite(bps) || bps <= 0) return '-';
  return fmtBytes(bps) + '/s';
}

function fmtBandwidth(bytesPerSec) {
  if (!isFinite(bytesPerSec) || bytesPerSec <= 0) return '-';
  const mbps = (bytesPerSec * 8) / 1e6;
  if (mbps >= 1000) return (mbps / 1000).toFixed(2) + ' Gbps';
  if (mbps >= 100) return mbps.toFixed(0) + ' Mbps';
  if (mbps >= 10) return mbps.toFixed(1) + ' Mbps';
  return mbps.toFixed(2) + ' Mbps';
}

function toast(msg, isErr) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast show' + (isErr ? ' err' : '');
  clearTimeout(t._h);
  t._h = setTimeout(() => { t.className = 'toast'; }, 2600);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ===== 初始化 ===== */

async function init() {
  try {
    const cfg = await (await fetch('/api/config')).json();
    if (cfg.title) { document.title = cfg.title + ' - 实时监测您的网络情况'; $('title').textContent = '🐴 ' + cfg.title; }
    $('foot').innerHTML = escapeHTML(cfg.footer || '');
    if (cfg.source_link) {
      $('srcOut').innerHTML = '源码地址：<a href="' + escapeAttr(cfg.source_link) + '" target="_blank" rel="noopener">' + escapeHTML(cfg.source_link) + '</a>';
    } else {
      $('srcOut').textContent = '本项目为自建单文件 Go 服务（前端内嵌）。\n\n' +
        '· 服务端：Go + net/http，无外部依赖\n' +
        '· 跨域方案：/api/proxy 同源代理，透传 Content-Length / Range\n' +
        '· 后台：/admin.html，测速链接可在后台增删改\n' +
        '· 数据：data.json（0600 权限）\n\n' +
        '管理员可在后台「站点设置 → 源码链接」填入仓库地址。';
    }
  } catch (e) {
    console.error(e);
  }

  await loadSources();
  bindEvents();
  loadWhoami();
}

async function loadSources() {
  try {
    const data = await (await fetch('/api/sources')).json();
    state.groups = data.groups || [];
    state.sources = [];
    const sel = $('src');
    sel.innerHTML = '';
    for (const g of state.groups) {
      const og = document.createElement('optgroup');
      og.label = g.name;
      for (const it of g.items) {
        const o = document.createElement('option');
        o.textContent = it.name;
        o.value = String(state.sources.length);
        og.appendChild(o);
        state.sources.push(it);
      }
      sel.appendChild(og);
    }
    if (state.sources.length === 0) {
      const o = document.createElement('option');
      o.textContent = '（后台还没有配置测速地址）';
      o.value = '';
      sel.appendChild(o);
    }
    state.src = state.sources[0] || null;
    if ($('auto').checked) start();
  } catch (e) {
    toast('加载测速地址失败：' + e.message, true);
  }
}

function bindEvents() {
  $('src').addEventListener('change', (e) => {
    const i = parseInt(e.target.value, 10);
    state.src = state.sources[i] || null;
  });

  $('threads').addEventListener('change', (e) => {
    let v = parseInt(e.target.value, 10);
    if (!isFinite(v) || v < 1) v = 1;
    if (v > 64) v = 64;
    e.target.value = v;
    state.threads = v;
    if (state.running) { toast('线程数将在下次开始时生效'); }
  });

  $('bg').addEventListener('change', (e) => { state.keepBg = e.target.checked; });

  $('auto').addEventListener('change', (e) => {
    if (e.target.checked && !state.running && state.src) start();
  });

  $('startBtn').addEventListener('click', () => {
    if (state.running) stop(); else start();
  });

  // 切到后台时，如果没勾「保持后台运行」就暂停
  document.addEventListener('visibilitychange', () => {
    if (document.hidden && state.running && !state.keepBg) {
      stop();
      toast('已切到后台，测速暂停（可开启「保持后台运行」）');
    }
  });

  window.addEventListener('beforeunload', (e) => {
    if (state.running) { e.preventDefault(); e.returnValue = ''; }
  });

  // Tabs
  document.querySelectorAll('.tabs button').forEach((b) => {
    b.addEventListener('click', () => {
      document.querySelectorAll('.tabs button').forEach((x) => x.classList.remove('active'));
      document.querySelectorAll('.tab-pane').forEach((x) => x.classList.remove('active'));
      b.classList.add('active');
      $('pane-' + b.dataset.tab).classList.add('active');
      if (b.dataset.tab === 'who') loadWhoami();
    });
  });

  $('ipBtn').addEventListener('click', queryIP);
  $('ipInput').addEventListener('keydown', (e) => { if (e.key === 'Enter') queryIP(); });
}

/* ===== 测速引擎 ===== */

function start() {
  if (state.running || !state.src) return;

  state.running = true;
  state.totalBytes = 0;
  state.startAt = performance.now();
  state.lastAt = state.startAt;
  state.lastBytes = 0;
  state.curSpeed = 0;
  state.avgSpeed = 0;
  state.samples = [];
  state.threads = parseInt($('threads').value, 10) || 8;
  state.aborts = [];

  $('startBtn').textContent = '停止';
  $('startBtn').classList.remove('primary');
  $('startBtn').classList.add('danger');
  $('hint').textContent = '正在测速：' + state.src.name + '（' + state.threads + ' 线程，经本机代理）';

  for (let i = 0; i < state.threads; i++) {
    const ctrl = new AbortController();
    state.aborts.push(ctrl);
    worker(ctrl.signal).catch(() => {});
  }

  state.ticker = setInterval(tick, TICK_MS);
  drawChart();
}

function stop() {
  if (!state.running) return;
  state.running = false;
  clearInterval(state.ticker);
  state.ticker = null;
  state.aborts.forEach((c) => { try { c.abort(); } catch (e) {} });
  state.aborts = [];

  $('startBtn').textContent = '开始测速';
  $('startBtn').classList.add('primary');
  $('startBtn').classList.remove('danger');
  $('hint').textContent = '已停止。总流量 ' + fmtBytes(state.totalBytes) +
    '，平均 ' + fmtSpeed(state.avgSpeed) + '。';
}

// worker 是一个线程的循环：不断地从代理端点拉数据，读完一轮再来一轮。
//
// 错误处理原则：**不要因为一个上游报错就把整个测速停掉**。
// 之前就是这里写错了 —— 上游 4xx 直接 stop()，用户看到「点开始立刻停止」。
// 实际上测速源失效是很常见的（链接过期、防盗链、超限），应该：
//   · 已经测到过流量 → 只让这个线程退避重试，整体继续；
//   · 一个字节都没测到且连续失败 → 才判定这个源不可用并停止，且把原因说清楚。
async function worker(signal) {
  const src = state.src;
  const limit = src.bytes && src.bytes > 0 ? src.bytes : 32 * 1024 * 1024;
  let consecutiveFail = 0;

  while (state.running && !signal.aborted) {
    const url = '/api/proxy?u=' + encodeURIComponent(src.url) + '&limit=' + limit;
    let res;
    try {
      res = await fetch(url, { signal, cache: 'no-store' });
    } catch (e) {
      if (!state.running || signal.aborted) return;
      consecutiveFail++;
      if (await giveUp(consecutiveFail, '网络请求失败：' + e.message)) return;
      await sleep(600);
      continue;
    }

    if (!res.ok || !res.body) {
      let msg = 'HTTP ' + res.status;
      const up = res.headers.get('X-Upstream-Status');
      if (up) msg += '（上游 ' + up + '）';
      try {
        const j = await res.json();
        if (j.error) msg += '：' + j.error;
      } catch (e) {}
      consecutiveFail++;
      if (await giveUp(consecutiveFail, msg)) return;
      await sleep(600);
      continue;
    }

    consecutiveFail = 0;
    const reader = res.body.getReader();
    try {
      while (state.running && !signal.aborted) {
        const { done, value } = await reader.read();
        if (done) break;
        state.totalBytes += value.byteLength;
      }
    } catch (e) {
      // abort 或网络中断，正常退出
    } finally {
      try { await reader.cancel(); } catch (e) {}
    }
  }
}

// giveUp 决定「这次失败要不要终止整个测速」。返回 true 表示已经停了。
async function giveUp(consecutiveFail, msg) {
  // 已经测到流量 → 说明源本身没问题，只是抖动，继续重试。
  if (state.totalBytes > 0) {
    if (consecutiveFail === 3) toast('上游间歇性报错，继续重试：' + msg, true);
    return false;
  }
  // 一个字节都没拿到，连续 3 次 → 判定源不可用。
  if (consecutiveFail >= 3) {
    // 注意顺序：stop() 会重写 #hint，所以必须先 stop 再写错误说明。
    stop();
    toast('测速源不可用：' + msg, true);
    $('hint').innerHTML = '<span class="err-text">测速源不可用：' + escapeHTML(msg) +
      '</span><br>这个地址可能已失效、有防盗链或限制。请在后台换一个源，或点「测试这个地址」先验证。';
    return true;
  }
  return false;
}

function tick() {
  const now = performance.now();
  const dt = (now - state.lastAt) / 1000;
  if (dt <= 0) return;

  const db = state.totalBytes - state.lastBytes;
  state.curSpeed = db / dt;
  const totalDt = (now - state.startAt) / 1000;
  state.avgSpeed = totalDt > 0 ? state.totalBytes / totalDt : 0;

  state.lastBytes = state.totalBytes;
  state.lastAt = now;

  state.samples.push(state.curSpeed);
  if (state.samples.length > MAX_SAMPLES) state.samples.shift();

  $('vTotal').innerHTML = fmtBytes(state.totalBytes);
  $('vCur').innerHTML = fmtSpeed(state.curSpeed);
  $('vAvg').innerHTML = fmtSpeed(state.avgSpeed);
  $('vBw').innerHTML = fmtBandwidth(state.curSpeed);

  // 用量预测：按当前平均速度线性外推
  const a = state.avgSpeed;
  $('fMin').textContent = fmtBytes(a * 60);
  $('fHour').textContent = fmtBytes(a * 3600);
  $('fDay').textContent = fmtBytes(a * 86400);
  $('fMon').textContent = fmtBytes(a * 86400 * 30);

  drawChart();
}

/* ===== 图表 ===== */

function drawChart() {
  const cv = $('chart');
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth;
  const h = cv.clientHeight;
  if (cv.width !== w * dpr || cv.height !== h * dpr) {
    cv.width = w * dpr;
    cv.height = h * dpr;
  }
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  const pad = 8;
  const innerW = w - pad * 2;
  const innerH = h - pad * 2;

  // 网格
  ctx.strokeStyle = '#eef0f3';
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad + (innerH / 4) * i;
    ctx.beginPath();
    ctx.moveTo(pad, y);
    ctx.lineTo(w - pad, y);
    ctx.stroke();
  }

  const s = state.samples;
  if (s.length < 2) {
    ctx.fillStyle = '#b6bcc4';
    ctx.font = '13px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('等待数据…', w / 2, h / 2);
    return;
  }

  const max = Math.max(...s, 1);
  const stepX = innerW / (MAX_SAMPLES - 1);

  // 面积
  const grad = ctx.createLinearGradient(0, pad, 0, h - pad);
  grad.addColorStop(0, 'rgba(58,109,240,.22)');
  grad.addColorStop(1, 'rgba(58,109,240,0)');
  ctx.beginPath();
  ctx.moveTo(pad, h - pad);
  s.forEach((v, i) => {
    const x = pad + stepX * i;
    const y = h - pad - (v / max) * innerH;
    ctx.lineTo(x, y);
  });
  ctx.lineTo(pad + stepX * (s.length - 1), h - pad);
  ctx.closePath();
  ctx.fillStyle = grad;
  ctx.fill();

  // 折线
  ctx.beginPath();
  s.forEach((v, i) => {
    const x = pad + stepX * i;
    const y = h - pad - (v / max) * innerH;
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.strokeStyle = '#3a6df0';
  ctx.lineWidth = 1.8;
  ctx.lineJoin = 'round';
  ctx.stroke();

  // 峰值标注
  ctx.fillStyle = '#8a919c';
  ctx.font = '11px ui-monospace, Menlo, monospace';
  ctx.textAlign = 'left';
  ctx.fillText('峰值 ' + fmtSpeed(max), pad + 4, pad + 12);
}

/* ===== Tabs 内容 ===== */

async function loadWhoami() {
  try {
    const d = await (await fetch('/api/whoami')).json();
    const lines = [];
    lines.push('Remote-Addr : ' + d.remote_addr);
    if (d.x_forwarded_for && d.x_forwarded_for.length) {
      lines.push('X-Forwarded-For : ' + d.x_forwarded_for.join(', '));
    }
    lines.push('User-Agent  : ' + d.user_agent);
    lines.push('Server-Time : ' + d.server_time);
    lines.push('');
    lines.push('--- 请求头 ---');
    Object.keys(d.headers).sort().forEach((k) => { lines.push(k + ': ' + d.headers[k]); });
    $('whoOut').textContent = lines.join('\n');
  } catch (e) {
    $('whoOut').textContent = '加载失败：' + e.message;
  }
}

async function queryIP() {
  const ip = $('ipInput').value.trim();
  $('ipOut').textContent = '查询中…';
  try {
    const d = await (await fetch('/api/ipinfo' + (ip ? '?ip=' + encodeURIComponent(ip) : ''))).json();
    if (d.error && !d.ok) { $('ipOut').textContent = d.error; return; }
    const lines = [];
    lines.push('IP      : ' + d.ip);
    lines.push('国家    : ' + (d.country || '-'));
    lines.push('省/州   : ' + (d.region || '-'));
    lines.push('城市    : ' + (d.city || '-'));
    lines.push('运营商  : ' + (d.isp || '-'));
    if (d.as) lines.push('AS      : ' + d.as);
    if (d.timezone) lines.push('时区    : ' + d.timezone);
    if (d.source) lines.push('数据源  : ' + d.source);
    $('ipOut').textContent = lines.join('\n');
  } catch (e) {
    $('ipOut').textContent = '查询失败：' + e.message;
  }
}

/* ===== 转义 ===== */

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function escapeAttr(s) { return escapeHTML(s); }

init();
