'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', usage: '用量', balhist: '余额趋势', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'balhist') loadBalance();
  if (v === 'taskscenter') { loadSchoolStatus(true); pollQueueOnce(); }
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');

/* ── 账号池 ───────────────────────────────────────────────────────── */
function renderAccounts(list) {
  const tb = $('accBody');
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
    const pct = s.credits_total > 0
      ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
      : Math.round((s.credits || 0) / maxCred * 100);
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip = s.credits_total > 0 ? '剩余 ' + s.credits + ' / 总额 ' + s.credits_total + '（' + pct + '%）' : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '">签到</button>' +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">余额</button>' +
        '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>' +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
  }).join('');
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
  $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* 模型页当前域。与余额页同款：两域的模型目录不同（global 有 gpt-5.3-codex 等
   只在 /v2 下发的模型），混在一起既看不到全貌也会串用数据。 */
let mdRealm = null;

/* renderMdRealmSeg 画模型页的域切换。只列**有账号的域**，账号数一并标出，
   让"这个域有没有号"一眼可见（该域无号时后端会 503，提前拦住）。 */
function renderMdRealmSeg(accounts) {
  const seg = $('mdRealmSeg');
  if (!seg) return;
  const count = {};
  (accounts || []).forEach(a => {
    const r = a.realm === 'global' ? 'global' : 'cn';
    count[r] = (count[r] || 0) + 1;
  });
  const items = BH_REALMS.filter(r => count[r.key]);
  if (!items.length) { seg.innerHTML = ''; return; }
  if (mdRealm === null || !count[mdRealm]) {
    const remembered = (function () { try { return localStorage.getItem('mdRealm'); } catch (e) { return null; } })();
    mdRealm = (remembered && count[remembered]) ? remembered : items[0].key;
  }
  seg.innerHTML = items.map(r =>
    '<button type="button" aria-pressed="' + (r.key === mdRealm ? 'true' : 'false') + '" data-realm="' + esc(r.key) + '">' +
    esc(r.label) + ' · ' + count[r.key] + '</button>').join('');
  Array.prototype.forEach.call(seg.querySelectorAll('button'), function (b) {
    b.onclick = function () {
      mdRealm = b.getAttribute('data-realm');
      try { localStorage.setItem('mdRealm', mdRealm); } catch (e) { /* 隐私模式：忽略 */ }
      loadModels();
    };
  });
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 账号列表用于画域切换（无号域提前禁用，避免点了才 503）
    let accts = [];
    try { const ov = await api('overview'); accts = ov.accounts || []; } catch (e) { /* 忽略 */ }
    renderMdRealmSeg(accts);
    const realm = mdRealm || 'cn';

    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([
      api('models?realm=' + encodeURIComponent(realm)),
      api('model_probes').catch(() => ({})),
    ]);
    const list = d.models || [];
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">该域上游未返回模型</div></td></tr>'; return; }
    const probes = pr.probes || {};
    // 探测键带域前缀（cn:glm-5.2）。**必须按当前域精确匹配**，不能用
    // endsWith(':'+id) 模糊找 —— 跨域同名模型（如两域都有 deepseek-v4.1）
    // 时 find() 只会命中第一个，导致 global 表格显示 cn 的探测结果。
    const probeOf = id => probes[realm + ':' + id] || probes[id];
    tb.innerHTML = list.map(m => {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
      if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + (m.credits ? esc(m.credits) : '—') + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    }).join('');
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = realmLabel(realm) + ' · ' + list.length + ' 个模型' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  try {
    const d = await api('logs');
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  max_body_mb: ['server', 'max_body_mb'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
};
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到选域态：选域可见、加载/就绪/完成/错误全收，起始按钮亮起。
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') pollQueueOnce();
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
const SCHOOL_META = [
  ['share_invite', '分享'],
  ['desktop_chat_1_time', '桌面'],
  ['chat_3_times', '对话×3'],
  ['expert_use', '专家'],
  ['task_student_verify', '认证'],
];
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
async function loadSchoolStatus(quiet) {
  const st = $('schoolState'), list = $('schoolList');
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('school/status');
    const arr = d.accounts || [];
    if (!arr.length) {
      st.hidden = false; st.className = 'state'; st.textContent = '暂无可用账号';
      list.innerHTML = ''; return;
    }
    let allDone = 0;
    const head = '<div class="shead"><div class="who">账号</div><div class="stasks">' +
      SCHOOL_META.map(([, name]) => '<span>' + esc(name) + '</span>').join('') +
      '</div><div class="luck">剩余抽奖</div></div>';
    list.innerHTML = head + arr.map(v => {
      const by = {};
      (v.tasks || []).forEach(t => by[t.task_code] = t);
      const cells = SCHOOL_META.map(([code]) => {
        const t = by[code];
        const html = code === 'task_student_verify'
          ? '<span class="stask todo"><span class="mark">—</span>不做</span>'
          : staskHTML(t);
        return '<span title="' + esc(SCHOOL_TITLES[code] || code) + '">' + html + '</span>';
      }).join('');
      const done = SCHOOL_META.filter(([code]) => code !== 'task_student_verify' && by[code] && by[code].status === 'claimed').length;
      allDone += done === 4 ? 1 : 0;
      return '<div class="srow">' +
        '<div class="who"><div class="nm" title="' + esc(v.nickname || '') + '">' + esc(v.nickname || '未命名') + '</div><div class="id">' + esc(v.uid) + '</div></div>' +
        '<div class="stasks">' + cells + '</div>' +
        '<div class="luck" title="剩余抽奖次数">' + LUCK_SVG + (v.chances == null ? '—' : v.chances) + '</div>' +
        (v.error ? '<div class="err">' + esc(v.error) + '</div>' : '') +
        '</div>';
    }).join('');
    $('schoolSummary').textContent = allDone === arr.length ? '今日全部完成 🎉' : allDone + '/' + arr.length + ' 个账号今日全部完成';
    st.hidden = true;
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};
$('btnSchoolRefresh').onclick = () => loadSchoolStatus(false);
$('btnSchoolRunAll').onclick = async () => {
  if (!confirm('将对全部账号执行开学季闭环（分享/桌面/对话/专家 + 抽奖），约 1-2 分钟。确认继续？')) return;
  try {
    await api('school/run_all', { method: 'POST' });
    toast('开学季闭环已开始，结果看任务日志', 'ok');
    setTimeout(() => loadSchoolStatus(true), 15000);
  } catch (e) { toast(e.message, 'err'); }
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    for (const t of (a.school || [])) {
      if (t.task_code === 'task_student_verify') continue; // 需真实认证，永不出现在待办
      rows.push({ kind: 'school', code: t.task_code, prog: t.target_count ? t.progress + '/' + t.target_count : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
async function pollQueueOnce() {
  try {
    const q = await api('tasks/queue');
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（q.running 时也要同代次——刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    renderQueue(groupsFromQueue(q.items || []), q);
  } catch (e) { /* 静默 */ }
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    await pollQueueOnce();
    try {
      const q = await api('tasks/queue');
      if (!q.running) {
        clearInterval(queueTimer); queueTimer = null;
        toast('任务队列执行结束', 'ok');
        loadSchoolStatus(true);
      }
    } catch (e) { /* 忽略 */ }
  }, 3000);
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

function usBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-wrapbar">' +
    '<span class="bar bar-p" style="width:' + (pp * 0.8).toFixed(1) + 'px" title="prompt"></span>' +
    '<span class="bar bar-c" style="width:' + Math.max(2, pc * 0.8).toFixed(1) + 'px" title="completion"></span>' +
    '</span>';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制是否追加延迟/速率两列——只有「按账号」表的表头带这两列；
   模型表与域表没有，多输出会造成列错位。早先靠「mid 是否为 undefined」隐式
   判断，调用方稍一改动就会错列，故改为显式参数。 */
function usRow(name, sub, a, mid, withPerf) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

function renderUsage(d) {
  const t = d.totals || {};
  $('usStats').innerHTML =
    usStat(fmtTok(t.requests), '请求数') +
    usStat(fmtTok(t.total_tokens), '总 token') +
    usStat(fmtTok(t.prompt_tokens), 'prompt') +
    usStat(fmtTok(t.completion_tokens), 'completion') +
    usStat(t.errors ? String(t.errors) : '0', '失败尝试', t.errors ? 'warn' : '') +
    usStat(fmtMs(t.avg_latency_ms), '平均延迟');

  $('usNote').textContent = (d.buckets || 0) + ' 个分桶 · ' +
    (d.since ? '自 ' + d.since.slice(0, 10) : '无数据') +
    (d.file_bytes ? ' · ' + (d.file_bytes / 1024).toFixed(1) + ' KB' : '');

  $('usAccBody').innerHTML = (d.by_account || []).map(x =>
    usRow(x.key.slice(0, 8), x.extra || '', x,
      '<td class="num">' + esc(x.realm || '') + '</td>', true)
  ).join('') || '<tr><td colspan="10" class="empty">暂无数据</td></tr>';

  $('usModelBody').innerHTML = (d.by_model || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  $('usRealmBody').innerHTML = (d.by_realm || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  renderUsageChart(d.series || []);
}

/* renderUsageChart 画堆叠柱状图。日点与小时点混用 x 轴，因此按数据序号等距
   排布（不按真实时间比例），并在标签上区分粒度——用量面板看的是相对高低，
   不是精确的时间刻度。 */
function renderUsageChart(series) {
  const host = $('usChart');
  if (!series.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    return;
  }
  const W = 760, H = 170, PL = 46, PR = 10, PT = 12, PB = 26;
  const iw = W - PL - PR, ih = H - PT - PB;

  const max = Math.max(1, ...series.map(p => Number(p.total_tokens || 0)));
  const bw = Math.max(2, Math.min(26, iw / series.length - 3));

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';
  // y 轴网格 + 刻度（4 档）
  for (let i = 0; i <= 4; i++) {
    const v = max * i / 4;
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y + '" x2="' + (W - PR) + '" y2="' + y + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5) + '" text-anchor="end">' + fmtTok(v) + '</text>';
  }
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) + '" y2="' + (PT + ih) + '"/>';

  const step = iw / series.length;
  series.forEach((p, i) => {
    const pt = Number(p.prompt_tokens || 0), ct = Number(p.completion_tokens || 0);
    const tt = Number(p.total_tokens || 0) || (pt + ct);
    const x = PL + i * step + (step - bw) / 2;
    const hTot = ih * (tt / max);
    const hP = tt ? hTot * (pt / tt) : 0;
    const hC = Math.max(tt && ct ? 1 : 0, hTot - hP);
    const yBase = PT + ih;
    if (hP > 0) out += '<rect x="' + x.toFixed(1) + '" y="' + (yBase - hP).toFixed(1) +
      '" width="' + bw.toFixed(1) + '" height="' + hP.toFixed(1) + '" fill="var(--accent)" rx="1.5"/>';
    if (hC > 0) out += '<rect x="' + x.toFixed(1) + '" y="' + (yBase - hP - hC).toFixed(1) +
      '" width="' + bw.toFixed(1) + '" height="' + hC.toFixed(1) + '" fill="var(--ok)" rx="1.5"/>';
    // 只给稀疏的几根画标签，避免拥挤
    const every = Math.ceil(series.length / 8);
    if (i % every === 0) {
      const lab = p.scope === 'day' ? p.t.slice(5) : p.t.slice(11) + ':00';
      out += '<text class="tk" x="' + (x + bw / 2).toFixed(1) + '" y="' + (H - 8) +
        '" text-anchor="middle">' + esc(lab) + '</text>';
    }
    out += '<title>' + esc(p.t) + ' (' + esc(p.scope) + ')  ' +
      fmtTok(p.prompt_tokens) + ' prompt / ' + fmtTok(p.completion_tokens) + ' completion / ' +
      (p.requests || 0) + ' 次</title>';
  });
  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

/* ── 余额趋势 ────────────────────────────────────────────────────────
   与「用量」视图的区别：用量是事件驱动的流量（柱状），余额是定时采样的存量
   水位（折线）。每账号一条线；涨跌用圆点标记，到账/消耗一眼可辨。 */
/* 账号身份色板。刻意**不含** var(--ok)/var(--bad) —— 绿/红已被「净增/净减」
   语义占用（每日变化柱），若账号色复用它们会产生双重编码冲突：同一颜色
   有两个互斥含义（"涨" vs "某账号"），读者必然误读。详见 §十一 同源纪律：
   一个视觉通道只承载一种含义。6 色循环，够覆盖单域常见账号数。 */
const BH_COLORS = ['var(--accent)', '#9b6dff', '#26b3c9', 'var(--warn)', '#e879a6', '#7d8cff'];

/* 域元数据：cn = 国内（CodeBuddy），global = 国际（WorkBuddy）。
   两域的积分不同源、不可互换（连上游 base 与模型目录都不同），
   因此**跨域求和语义不成立** —— 余额趋势与模型页都必须分域展示，
   不做"国内 + 国际"的全局总计。 */
const BH_REALMS = [
  { key: 'cn', label: '国内' },
  { key: 'global', label: '国际' },
];
/* bhRealm 当前选中的域。null = 尚未初始化（首次加载时按数据自动选一个非空域）。 */
let bhRealm = null;

/* realmOf 归一化账号域：空/未知归入 'other'（不静默丢弃，让它可见）。 */
function realmOf(s) {
  const r = (s && s.realm) || '';
  return r === 'cn' || r === 'global' ? r : 'other';
}

/* pickRealm 选择默认域：优先记住的选择，其次第一个有数据的已知域。 */
function pickRealm(rows) {
  const have = {};
  rows.forEach(s => { have[realmOf(s)] = true; });
  const remembered = (function () {
    try { return localStorage.getItem('bhRealm'); } catch (e) { return null; }
  })();
  if (remembered && have[remembered]) return remembered;
  const first = BH_REALMS.find(r => have[r.key]);
  if (first) return first.key;
  return have.other ? 'other' : (BH_REALMS[0] && BH_REALMS[0].key) || 'cn';
}

/* filterRealm 取当前域的账号行。 */
function filterRealm(rows, realm) {
  return rows.filter(s => realmOf(s) === realm);
}

/* renderBalanceChart 画多账号余额折线。
   x 轴按**真实时间比例**排布（30 分钟等间隔），而非数据序号等距——
   因为"什么时候到账/掉分"本身就是这张图要回答的问题。 */
function renderBalanceChart(series) {
  const host = $('bhChart');
  const rows = (series || []).filter(s => (s.points || []).length >= 1);
  if (!rows.length) {
    host.innerHTML = '<div class="us-empty">暂无余额采样。服务每 30 分钟自动记录一点，' +
      '余额刷新后即出现（也可点上方「刷新」立即采样一次）。</div>';
    return;
  }
  let t0 = Infinity, t1 = -Infinity, lo = Infinity, hi = -Infinity;
  rows.forEach(s => s.points.forEach(p => {
    const t = Number(p.t) * 1000;
    if (t < t0) t0 = t;
    if (t > t1) t1 = t;
    const c = Number(p.c);
    if (c < lo) lo = c;
    if (c > hi) hi = c;
  }));
  if (t1 <= t0) t1 = t0 + 1;                  // 单点：给最小跨度，避免除零
  if (hi <= lo) hi = lo + 1;                  // 水平线：撑开一点，否则贴边
  // 上下留白 12%，但 y 轴下界不得低于 0 —— 余额永远非负，画出负刻度是误导。
  // （曾因 lo 下推过多而显示 "-443" 这样的负刻度，用户会以为余额为负。）
  const pad = (hi - lo) * 0.12;
  lo = Math.max(0, lo - pad);
  hi += pad;

  const W = 760, H = 200, PL = 52, PR = 12, PT = 12, PB = 26;
  const iw = W - PL - PR, ih = H - PT - PB;
  const X = t => PL + iw * ((t - t0) / (t1 - t0));
  const Y = c => PT + ih - ih * ((c - lo) / (hi - lo));

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';
  for (let i = 0; i <= 4; i++) {              // y 轴 4 档刻度
    const v = lo + (hi - lo) * i / 4;
    const y = Y(v);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' +
      fmtNum(Math.round(v)) + '</text>';
  }
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) + '" y2="' + (PT + ih) + '"/>';

  const spanDays = (t1 - t0) / 86400000;      // x 轴标签：首/中/末三点
  const fmtT = t => {
    const d = new Date(t);
    const mm = String(d.getMonth() + 1).padStart(2, '0');
    const dd = String(d.getDate()).padStart(2, '0');
    if (spanDays <= 2) return mm + '-' + dd + ' ' + String(d.getHours()).padStart(2, '0') + ':00';
    return mm + '-' + dd;
  };
  [t0, (t0 + t1) / 2, t1].forEach((t, i) => {
    out += '<text class="tk" x="' + X(t).toFixed(1) + '" y="' + (H - 8) +
      '" text-anchor="' + (i === 0 ? 'start' : i === 2 ? 'end' : 'middle') + '">' + esc(fmtT(t)) + '</text>';
  });

  rows.forEach((s, si) => {
    const color = BH_COLORS[si % BH_COLORS.length];
    const pts = s.points.map(p => ({ x: X(Number(p.t) * 1000), y: Y(Number(p.c)), c: Number(p.c), t: Number(p.t) * 1000, g: p.d }));
    out += '<polyline fill="none" stroke="' + color + '" stroke-width="1.8" stroke-linejoin="round" ' +
      'stroke-linecap="round" points="' + pts.map(p => p.x.toFixed(1) + ',' + p.y.toFixed(1)).join(' ') + '"/>';
    pts.forEach((p, i) => {
      const prev = i > 0 ? pts[i - 1] : null;
      const delta = prev ? p.c - prev.c : 0;
      if (prev && delta !== 0) {              // 涨跌标记：涨=实心绿、跌=小号红
        const up = delta > 0;
        out += '<circle cx="' + p.x.toFixed(1) + '" cy="' + p.y.toFixed(1) + '" r="' + (up ? 3 : 2.2) +
          '" fill="' + (up ? 'var(--ok)' : 'var(--bad)') + '" stroke="' + color + '" stroke-width="1"/>';
      } else if (!prev) {
        out += '<circle cx="' + p.x.toFixed(1) + '" cy="' + p.y.toFixed(1) + '" r="2.2" fill="' + color + '"/>';
      }
      const when = new Date(p.t).toLocaleString('zh-CN', { hour12: false });
      const head = esc(s.name || s.uid.slice(0, 8)) + '  ' + esc(when) + (p.g ? '（日粒度）' : '');
      out += '<title>' + head + '\n余额 ' + fmtNum(p.c) +
        (prev ? '\n较上一点 ' + (delta > 0 ? '+' : '') + fmtNum(delta) : '') + '</title>';
    });
    const last = pts[pts.length - 1];          // 末点加大命中圈，便于悬停
    out += '<circle cx="' + last.x.toFixed(1) + '" cy="' + last.y.toFixed(1) + '" r="9" fill="transparent">' +
      '<title>' + esc(s.name || s.uid.slice(0, 8)) + '  当前 ' + fmtNum(last.c) + '</title></circle>';
  });
  out += '</svg>';
  host.innerHTML = out;

  const lg = $('bhLegend');
  if (lg) lg.innerHTML = rows.map((s, i) =>
    '<i class="sw" style="background:' + BH_COLORS[i % BH_COLORS.length] + '"></i>' +
    esc(s.name || s.uid.slice(0, 8))).join(' ');
}


/* renderDailyBars 画「每日变化」分组柱状图：x = 日期，每账号一根。
   与余额折线的区别：折线看"水位随时间"，这里看"每天的净变化量"（涨跌方向 + 幅度）。
   不完整日（今日 / 服务中途启用那天）用低透明度 + 虚线描边标出 —— 否则
   「今天只涨了一点」会被误读成「今天赚得少」，其实是当天还没采完。 */
function renderDailyBars(series) {
  const host = $('bhDaily');
  if (!host) return;
  // 收集所有日期（并集，升序）
  const daySet = new Set();
  series.forEach(s => (s.daily || []).forEach(d => daySet.add(d.day)));
  const days = [...daySet].sort();
  if (!days.length) {
    host.innerHTML = '<div class="us-empty">暂无按日数据（需跨过零点后才出现第一天）。</div>';
    const lg = $('bhDailyLegend');
    if (lg) lg.innerHTML = '';
    return;
  }
  // 每个账号每天的变化值，以及是否有任一不完整
  const map = new Map(); // uid -> {day: {change, full}}
  const fullMap = new Map();
  series.forEach(s => {
    const m = {};
    (s.daily || []).forEach(d => { m[d.day] = d.change; if (!d.full) fullMap.set(s.uid + '|' + d.day, true); });
    map.set(s.uid, m);
  });
  // y 轴范围：正负分开，零轴居中
  let hi = 0, lo = 0;
  series.forEach(s => (s.daily || []).forEach(d => {
    if (d.change > hi) hi = d.change;
    if (d.change < lo) lo = d.change;
  }));
  // 上下独立缩放 + 各留 12% 余量（不再强制对称）。
  // 为什么：强制对称（hi=lo=-span）在"几乎只涨"的数据上会把下半区留成
  // 5 倍空白（实测 +1998/-393 → 轴 ±1998），视觉上又空又怪，且正柱贴顶。
  // 独立缩放让柱高比例真实反映数值比例。
  // 边界断言（§十：不得产出物理上不可能的值）：日变化可正可负，故只在
  // 该侧确有数据时才外扩；两侧都无数据时兜底 ±1 避免除零。
  if (hi > 0) hi *= 1.12;
  if (lo < 0) lo *= 1.12;
  // 单侧有数据时，给另一侧留一点呼吸空间，否则零轴贴边、柱子像被裁掉。
  if (hi > 0 && lo === 0) lo = -Math.max(1, hi * 0.15);
  if (lo < 0 && hi === 0) hi = Math.max(1, -lo * 0.15);
  if (hi === 0 && lo === 0) { hi = 1; lo = -1; }

  const W = 760, H = 180, PL = 56, PR = 12, PT = 14, PB = 34;
  const iw = W - PL - PR, ih = H - PT - PB;
  const Y = v => PT + ih - ih * ((v - lo) / (hi - lo));
  const zeroY = Y(0);

  const nAcc = Math.max(1, series.length);
  const groupW = iw / days.length;
  const bw = Math.max(2, Math.min(14, (groupW - 8 - Math.max(0, nAcc - 1)) / nAcc));

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';
  // 网格（4 档）+ 零轴加粗
  for (let i = 0; i <= 4; i++) {
    const v = lo + (hi - lo) * i / 4;
    const y = Y(v);
    const isZero = Math.abs(v) < Math.max(hi, -lo) / 8;
    out += '<line class="' + (isZero ? 'ax' : 'gl') + '" x1="' + PL + '" y1="' + y.toFixed(1) +
      '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' +
      (v > 0 ? '+' : '') + fmtNum(Math.round(v)) + '</text>';
  }
  // 每日一组柱
  days.forEach((day, di) => {
    const gx = PL + di * groupW;
    series.forEach((s, si) => {
      const v = (map.get(s.uid) || {})[day];
      if (v === undefined) return;
      const color = BH_COLORS[si % BH_COLORS.length];
      // 组内柱之间留 1px 缝：无缝隙时相邻两柱会被知觉成一个整体
      // （格式塔共边界效应），看不出"两根独立的柱"。
      const gap = nAcc > 1 ? 1 : 0;
      const x = gx + (groupW - (bw * nAcc + gap * (nAcc - 1))) / 2 + si * (bw + gap);
      const y0 = Math.min(zeroY, Y(v)), h = Math.max(1, Math.abs(Y(v) - zeroY));
      const incomplete = fullMap.get(s.uid + '|' + day);
      // 零值画一条中性底痕（与 buka 规范一致：0 值也要可见）
      if (v === 0) {
        out += '<rect x="' + x.toFixed(1) + '" y="' + (zeroY - 1).toFixed(1) + '" width="' + bw.toFixed(1) +
          '" height="2" fill="var(--line)" rx="1"/>';
      } else {
        out += '<rect x="' + x.toFixed(1) + '" y="' + y0.toFixed(1) + '" width="' + bw.toFixed(1) +
          '" height="' + h.toFixed(1) + '" fill="' + (v > 0 ? 'var(--ok)' : 'var(--bad)') + '"' +
          (incomplete ? ' opacity=".38" stroke="' + color + '" stroke-width="1" stroke-dasharray="2 2"' : '') +
          ' rx="1.5"/>';
      }
      out += '<title>' + esc(s.name || s.uid.slice(0, 8)) + '  ' + esc(day) + '\n' +
        (v > 0 ? '+' : '') + fmtNum(v) + (incomplete ? '\n⚠️ 该日采样不完整（当天未结束或中途启用）' : '') + '</title>';
    });
    // x 轴日期标签（稀疏）
    const every = Math.ceil(days.length / 10);
    if (di % every === 0) {
      out += '<text class="tk" x="' + (gx + groupW / 2).toFixed(1) + '" y="' + (H - 12) +
        '" text-anchor="middle">' + esc(day.slice(5)) + '</text>';
    }
  });
  out += '</svg>';
  host.innerHTML = out;

  // 图例：柱色表示**涨跌**（绿=净增、红=净减），不表示账号——账号由组内
  // 左→右的位置区分（与下方表格行序一致）。故这里只说明颜色语义，
  // 再按位置列出账号顺序，避免"颜色=账号"的错误暗示。
  const lg = $('bhDailyLegend');
  if (lg) lg.innerHTML =
    '<span class="lg-key"><i class="sw" style="background:var(--ok)"></i>净增</span>' +
    '<span class="lg-key"><i class="sw" style="background:var(--bad)"></i>净减</span>' +
    '<span class="lg-sep"></span>' +
    '<span class="lg-order">组内左→右：' +
    series.map((s, i) => (i + 1) + '. ' + esc(s.name || s.uid.slice(0, 8))).join('　') +
    '</span>';
}

/* 取某账号的「今日/昨日」变化（用于表格列）。 */
function dayChangeOf(s, which) {
  const d = s.daily || [];
  if (!d.length) return null;
  if (which === 'today') return d[d.length - 1];
  return d.length >= 2 ? d[d.length - 2] : null;
}

function fmtChange(v, full) {
  if (v === null || v === undefined) return '—';
  const txt = (v > 0 ? '+' : '') + fmtNum(v);
  const color = v > 0 ? 'var(--ok)' : v < 0 ? 'var(--bad)' : 'inherit';
  // 不完整日用虚线底 + 提示，避免与完整日直接比较
  return '<span style="color:' + color + (full ? '' : '; opacity:.6') + '"' +
    (full ? '' : ' title="该日采样不完整（当天未结束或中途启用），不宜与完整日直接比较"') + '>' + txt + '</span>';
}

/* fmtFlow 渲染「消耗/到账」这类恒非负的流量值。
   与 fmtChange 的区别：这些量不表示方向（消耗恒 >=0），故不用涨跌色，
   用中性色 + 弱化不完整日。0 显示为 "–"（无变化比 "0" 更易扫读）。 */
function fmtFlow(v, full) {
  if (v === null || v === undefined) return '—';
  const n = Number(v);
  if (!n) return '<span style="color:var(--ink-3)">–</span>';
  return '<span style="color:var(--ink-2)' + (full ? '' : '; opacity:.6') + '"' +
    (full ? '' : ' title="该日采样不完整，消耗/到账均为下界估计，不宜与完整日直接比较"') + '>' +
    fmtNum(n) + '</span>';
}

function fmtNum(v) {
  const n = Number(v) || 0;
  return n.toLocaleString('zh-CN');
}

function bhCard(k, v, tone) {
  return '<div class="st"><span class="k">' + esc(k) + '</span><span class="v' +
    (tone ? ' ' + tone : '') + '">' + esc(v) + '</span></div>';
}

async function loadBalance() {
  const host = $('bhChart');
  const days = Number(($('bhWindow') || {}).value || 30);
  try {
    const d = await api('balance_history?days=' + days);
    const all = d.series || [];
    // 首次进入（或记忆的域已无数据）时确定默认域；之后保持用户的显式选择。
    if (bhRealm === null || !all.some(s => realmOf(s) === bhRealm)) {
      bhRealm = pickRealm(all);
    }
    renderRealmSeg(all);
    const rows = filterRealm(all, bhRealm);
    renderBalanceChart(rows);
    renderDailyBars(rows);
    // 分域小计：同域内可加（语义成立）；**不做跨域总计**——cn 与 global 的
    // 积分不同源、不可互换，相加得出的数没有意义（见 BH_REALMS 注释）。
    let total = 0, delta = 0;
    rows.forEach(s => { total += Number(s.current || 0); delta += Number(s.change || 0); });
    const st = $('bhStats');
    if (st) st.innerHTML =
      bhCard('账号', String(rows.length)) +
      bhCard('当前合计', fmtNum(total)) +
      bhCard('窗口净变化', (delta > 0 ? '+' : '') + fmtNum(delta), delta > 0 ? 'ok' : delta < 0 ? 'bad' : '') +
      bhCard('采样点数', fmtNum(rows.reduce((a, s) => a + (s.points || []).length, 0)));
    const tb = $('bhAccBody');
    if (tb) tb.innerHTML = rows.length ? rows.map((s, i) =>
      '<tr><td class="mark"><i class="dot" style="background:' + BH_COLORS[i % BH_COLORS.length] + '"></i></td>' +
      '<td>' + esc(s.name || s.uid.slice(0, 8)) + '</td>' +
      '<td class="num">' + fmtNum(s.current) + '</td>' +
      '<td class="num">' + fmtNum(s.min) + '</td>' +
      '<td class="num">' + fmtNum(s.max) + '</td>' +
      '<td class="num" style="color:' + (s.change > 0 ? 'var(--ok)' : s.change < 0 ? 'var(--bad)' : 'inherit') + '">' +
        (s.change > 0 ? '+' : '') + fmtNum(s.change) + '</td>' +
      (function () {
        const t = dayChangeOf(s, 'today'), y = dayChangeOf(s, 'yesterday');
        return '<td class="num">' + fmtChange(t && t.change, t && t.full) + '</td>' +
               '<td class="num">' + fmtChange(y && y.change, y && y.full) + '</td>' +
               // 今日消耗/到账：不完整日按弱化显示（口径与今日列一致）
               '<td class="num">' + fmtFlow(t && t.consumed, t && t.full) + '</td>' +
               '<td class="num">' + fmtFlow(t && t.gained, t && t.full) + '</td>';
      })() +
      '<td class="num">' + (s.points || []).length + '</td></tr>').join('')
      : '<tr><td colspan="11"><div class="empty"><div class="big">该域暂无采样</div>' +
        '切到其他域，或等待下一次余额刷新</div></td></tr>';
    renderBalanceFoot(rows);
    const note = $('bhNote');
    if (note) note.textContent = '更新于 ' + new Date(d.now).toLocaleTimeString('zh-CN', { hour12: false });
  } catch (e) {
    if (host) host.innerHTML = '<div class="us-empty">读取余额趋势失败：' + esc(e.message) + '</div>';
  }
}

/* renderBalanceFoot 画「按账号」表的合计行（**域内小计**）。
   口径纪律（§十：不得产出语义错误的数）：
     - 当前 / 净变化 / 今日 / 昨日 / 采样点 —— 可加，给出合计；
     - 最低 / 最高 —— **不可加**！各账号的低谷/高峰不在同一时刻，
       Σ最低 没有任何物理含义（会得出一个从未存在过的水位），故填 "—"。
   不做跨域总计：cn 与 global 的积分不同源、不可互换（见 BH_REALMS）。 */
function renderBalanceFoot(rows) {
  const host = $('bhAccFoot');
  if (!host) return;
  if (!rows.length) { host.innerHTML = ''; return; }
  let cur = 0, chg = 0, pts = 0;
  let todaySum = 0, todayHas = false, ydaySum = 0, ydayHas = false;
  let consSum = 0, consHas = false, gainSum = 0, gainHas = false;
  rows.forEach(s => {
    cur += Number(s.current || 0);
    chg += Number(s.change || 0);
    pts += (s.points || []).length;
    const t = dayChangeOf(s, 'today'), y = dayChangeOf(s, 'yesterday');
    if (t && typeof t.change === 'number') { todaySum += t.change; todayHas = true; }
    if (y && typeof y.change === 'number') { ydaySum += y.change; ydayHas = true; }
    // 消耗/到账可跨账号相加（都是同域的绝对量），但**同为下界估计**
    if (t && typeof t.consumed === 'number') { consSum += t.consumed; consHas = true; }
    if (t && typeof t.gained === 'number') { gainSum += t.gained; gainHas = true; }
  });
  const num = (v, has) => has
    ? '<td class="num" style="color:' + (v > 0 ? 'var(--ok)' : v < 0 ? 'var(--bad)' : 'inherit') + '">' +
      (v > 0 ? '+' : '') + fmtNum(v) + '</td>'
    : '<td class="num na">—</td>';
  host.innerHTML = '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>合计（' + esc(realmLabel(bhRealm)) + '）</td>' +
    '<td class="num">' + fmtNum(cur) + '</td>' +
    // 最低/最高不求和——见函数头注释
    '<td class="num na" title="各账号低谷不在同一时刻，不可相加">—</td>' +
    '<td class="num na" title="各账号高峰不在同一时刻，不可相加">—</td>' +
    '<td class="num" style="color:' + (chg > 0 ? 'var(--ok)' : chg < 0 ? 'var(--bad)' : 'inherit') + '">' +
      (chg > 0 ? '+' : '') + fmtNum(chg) + '</td>' +
    num(todaySum, todayHas) + num(ydaySum, ydayHas) +
    flowCell(consSum, consHas) + flowCell(gainSum, gainHas) +
    '<td class="num">' + fmtNum(pts) + '</td>' +
    '</tr>';
}

/* flowCell 合计行的流量单元格（消耗/到账）。无数据显示 —，有数据显示数值。
   注意：合计值同为**下界估计**（各账号的采样漏掉量不因求和而补回）。 */
function flowCell(v, has) {
  return has
    ? '<td class="num">' + (v ? fmtNum(v) : '<span style="color:var(--ink-3)">–</span>') + '</td>'
    : '<td class="num na">—</td>';
}

/* realmLabel 域 key 到展示名。 */
function realmLabel(key) {
  const r = BH_REALMS.find(x => x.key === key);
  return r ? r.label : '其他';
}

/* renderRealmSeg 画域切换分段控件。只列出**有数据的域**（避免点了空白页），
   并在按钮上标明账号数，让"这个域有几个号"一眼可见。 */
function renderRealmSeg(all) {
  const seg = $('bhRealmSeg');
  if (!seg) return;
  const count = {};
  all.forEach(s => { const r = realmOf(s); count[r] = (count[r] || 0) + 1; });
  const items = BH_REALMS.filter(r => count[r.key]).concat(
    count.other ? [{ key: 'other', label: '其他' }] : []);
  if (!items.length) { seg.innerHTML = ''; return; }
  seg.innerHTML = items.map(r =>
    '<button type="button" aria-pressed="' + (r.key === bhRealm ? 'true' : 'false') + '" data-realm="' + esc(r.key) + '">' +
    esc(r.label) + ' · ' + count[r.key] + '</button>').join('');
  Array.prototype.forEach.call(seg.querySelectorAll('button'), function (b) {
    b.onclick = function () {
      bhRealm = b.getAttribute('data-realm');
      try { localStorage.setItem('bhRealm', bhRealm); } catch (e) { /* 隐私模式：忽略 */ }
      loadBalance();
    };
  });
}

if ($('bhWindow')) $('bhWindow').onchange = loadBalance;

async function loadUsage() {
  const hours = ($('usWindow') && $('usWindow').value) || 72;
  try {
    const d = await api('usage?hours=' + encodeURIComponent(hours));
    renderUsage(d);
  } catch (e) {
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
if ($('usWindow')) $('usWindow').onchange = loadUsage;

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

function renderPackages(d) {
  const list = (d.accounts || []);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，包的**面额**列是重点
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const packs = (a.packages || []);
    const rows = packs.map(p => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    }).join('');
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · ' + packs.length + ' 个包（按面额降序）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div></div>';
  }).join('');
}

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  try {
    const d = await api('packages');
    renderPackages(d);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnPk')) $('btnPk').onclick = loadPackages;
