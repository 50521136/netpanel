/* 后台管理逻辑 */
'use strict';

const $ = (id) => document.getElementById(id);

let sources = [];
let editingId = null;   // null = 新增

function toast(msg, isErr) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast show' + (isErr ? ' err' : '');
  clearTimeout(t._h);
  t._h = setTimeout(() => { t.className = 'toast'; }, 2600);
}

async function api(path, opts) {
  const res = await fetch(path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts || {}));
  let body = null;
  try { body = await res.json(); } catch (e) {}
  if (!res.ok) {
    const msg = (body && body.error) || ('HTTP ' + res.status);
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return body;
}

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n <= 0) return '默认';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
}

/* ===== 启动 ===== */

async function boot() {
  try {
    const me = await api('/api/me');
    if (me.authed) showAdmin(); else showLogin();
  } catch (e) {
    showLogin();
  }
  bind();
}

function showLogin() {
  $('loginView').classList.remove('hidden');
  $('adminView').classList.add('hidden');
  $('pw').focus();
}

async function showAdmin() {
  $('loginView').classList.add('hidden');
  $('adminView').classList.remove('hidden');
  await Promise.all([loadSources(), loadSettings()]);
}

function bind() {
  $('loginBtn').addEventListener('click', doLogin);
  $('pw').addEventListener('keydown', (e) => { if (e.key === 'Enter') doLogin(); });

  $('logoutBtn').addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' });
    showLogin();
  });

  $('addBtn').addEventListener('click', () => openEditor(null));
  $('cancelBtn').addEventListener('click', closeEditor);
  $('saveBtn').addEventListener('click', saveSource);
  $('testBtn').addEventListener('click', testSource);

  $('saveSettings').addEventListener('click', saveSettings);
  $('savePw').addEventListener('click', savePassword);
}

async function doLogin() {
  const pw = $('pw').value;
  if (!pw) { toast('请输入密码', true); return; }
  $('loginBtn').disabled = true;
  try {
    await api('/api/login', { method: 'POST', body: JSON.stringify({ password: pw }) });
    $('pw').value = '';
    $('loginHint').textContent = '';
    showAdmin();
  } catch (e) {
    $('loginHint').textContent = e.message;
    $('loginHint').className = 'hint err-text';
  } finally {
    $('loginBtn').disabled = false;
  }
}

/* ===== 测速源 ===== */

async function loadSources() {
  const d = await api('/api/admin/sources');
  sources = d.sources || [];
  renderSources();
}

function renderSources() {
  const tb = $('srcBody');
  tb.innerHTML = '';
  $('srcCount').textContent = '共 ' + sources.length + ' 条';

  // 分类候选
  const cats = [...new Set(sources.map((s) => s.category).filter(Boolean))];
  $('catList').innerHTML = cats.map((c) => '<option value="' + esc(c) + '">').join('');

  if (sources.length === 0) {
    tb.innerHTML = '<tr><td colspan="6" style="color:#8a919c;text-align:center;padding:24px">还没有测速链接，点右上角「新增」</td></tr>';
    return;
  }

  for (const s of sources) {
    const tr = document.createElement('tr');
    tr.innerHTML =
      '<td>' + esc(s.name) + '</td>' +
      '<td>' + (s.category ? '<span class="badge">' + esc(s.category) + '</span>' : '-') + '</td>' +
      '<td class="url" title="' + esc(s.url) + '">' + esc(s.url) + '</td>' +
      '<td>' + fmtBytes(s.bytes) + '</td>' +
      '<td>' + (s.enabled ? '<span class="badge">启用</span>' : '<span class="badge off">停用</span>') + '</td>' +
      '<td></td>';

    const act = tr.lastElementChild;
    const bEdit = document.createElement('button');
    bEdit.className = 'sm';
    bEdit.textContent = '编辑';
    bEdit.onclick = () => openEditor(s.id);
    const bDel = document.createElement('button');
    bDel.className = 'sm danger';
    bDel.textContent = '删除';
    bDel.style.marginLeft = '6px';
    bDel.onclick = () => delSource(s.id, s.name);
    act.appendChild(bEdit);
    act.appendChild(bDel);
    tb.appendChild(tr);
  }
}

function openEditor(id) {
  editingId = id;
  const s = id ? sources.find((x) => x.id === id) : null;
  $('editCard').classList.remove('hidden');
  $('editTitle').textContent = s ? ('编辑：' + s.name) : '新增测速链接';
  $('fName').value = s ? s.name : '';
  $('fCat').value = s ? (s.category || '') : '';
  $('fUrl').value = s ? s.url : '';
  $('fBytes').value = s ? s.bytes : 33554432;
  $('fEnabled').value = s && !s.enabled ? '0' : '1';
  $('testOut').textContent = '';
  $('editCard').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

function closeEditor() {
  editingId = null;
  $('editCard').classList.add('hidden');
}

async function saveSource() {
  const payload = {
    name: $('fName').value.trim(),
    category: $('fCat').value.trim(),
    url: $('fUrl').value.trim(),
    bytes: parseInt($('fBytes').value, 10) || 0,
    enabled: $('fEnabled').value === '1',
  };
  if (!payload.name) { toast('名称不能为空', true); return; }
  if (!/^https?:\/\//i.test(payload.url)) { toast('地址必须以 http:// 或 https:// 开头', true); return; }

  try {
    if (editingId) {
      await api('/api/admin/sources/' + encodeURIComponent(editingId), { method: 'PUT', body: JSON.stringify(payload) });
    } else {
      await api('/api/admin/sources', { method: 'POST', body: JSON.stringify(payload) });
    }
    toast('已保存');
    closeEditor();
    await loadSources();
  } catch (e) {
    toast(e.message, true);
  }
}

async function delSource(id, name) {
  if (!confirm('确定删除「' + name + '」？')) return;
  try {
    await api('/api/admin/sources/' + encodeURIComponent(id), { method: 'DELETE' });
    toast('已删除');
    if (editingId === id) closeEditor();
    await loadSources();
  } catch (e) {
    toast(e.message, true);
  }
}

// 用代理端点拉 1MB 试试通不通
async function testSource() {
  const url = $('fUrl').value.trim();
  if (!/^https?:\/\//i.test(url)) { toast('先填一个合法地址', true); return; }
  $('testOut').textContent = '测试中…';
  const t0 = performance.now();
  try {
    const res = await fetch('/api/proxy?u=' + encodeURIComponent(url) + '&limit=1048576');
    if (!res.ok) {
      let msg = 'HTTP ' + res.status;
      try { const j = await res.json(); if (j.error) msg += '：' + j.error; } catch (e) {}
      $('testOut').textContent = '✗ ' + msg;
      $('testOut').className = 'hint err-text';
      return;
    }
    const reader = res.body.getReader();
    let got = 0;
    while (got < 1048576) {
      const { done, value } = await reader.read();
      if (done) break;
      got += value.byteLength;
    }
    try { await reader.cancel(); } catch (e) {}
    const ms = performance.now() - t0;
    const spd = got / (ms / 1000);
    $('testOut').textContent = '✓ 通了：' + fmtBytes(got) + ' / ' + ms.toFixed(0) + 'ms，约 ' + fmtBytes(spd) + '/s';
    $('testOut').className = 'hint';
  } catch (e) {
    $('testOut').textContent = '✗ ' + e.message;
    $('testOut').className = 'hint err-text';
  }
}

/* ===== 站点设置 ===== */

async function loadSettings() {
  try {
    const d = await api('/api/admin/settings');
    $('sTitle').value = d.title || '';
    $('sFooter').value = d.footer || '';
    $('sLink').value = d.source_link || '';
  } catch (e) {
    toast('加载设置失败：' + e.message, true);
  }
}

async function saveSettings() {
  try {
    await api('/api/admin/settings', {
      method: 'POST',
      body: JSON.stringify({
        title: $('sTitle').value.trim(),
        footer: $('sFooter').value.trim(),
        source_link: $('sLink').value.trim(),
      }),
    });
    toast('设置已保存');
  } catch (e) {
    toast(e.message, true);
  }
}

async function savePassword() {
  const old = $('pOld').value, nw = $('pNew').value;
  if (!old || !nw) { toast('请填写原密码与新密码', true); return; }
  if (nw.length < 6) { toast('新密码至少 6 位', true); return; }
  try {
    await api('/api/admin/password', { method: 'POST', body: JSON.stringify({ old, new: nw }) });
    $('pOld').value = ''; $('pNew').value = '';
    toast('密码已修改');
  } catch (e) {
    toast(e.message, true);
  }
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

boot();
