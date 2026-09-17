/* logo.js 候选 logo 的 SVG 与渲染（独立文件，配合严格 CSP：无内联脚本）。
   全部用 currentColor 单色，随主题自动变色。 */
(function () {
  'use strict';

  // 每个候选项：id 与 svg 片段（viewBox 0 0 48 48）
  var L = [
    ['1', '池 · 三层环', '「账号池」水面意象：内实外深、外两圈渐虚，表达「有中心、有边界」',
      '<circle cx="24" cy="24" r="17" stroke="currentColor" stroke-width="2" opacity=".28"/>' +
      '<circle cx="24" cy="24" r="11.5" stroke="currentColor" stroke-width="2" opacity=".55"/>' +
      '<circle cx="24" cy="24" r="6" fill="currentColor"/>'],
    ['2', 'W 折线', '呼应 WorkBuddy2API 首字母，单字重、远距离辨识度最高',
      '<path d="M9 13 L17 35 L24 21 L31 35 L39 13" stroke="currentColor" stroke-width="3.2" ' +
      'stroke-linecap="round" stroke-linejoin="round"/>'],
    ['3', '汇聚路由', '三个来源汇入一个出口 —— 直接表达「多账号 → 网关」',
      '<path d="M10 12h8c4 0 6 3 6 7v10c0 4 2 7 6 7h8" stroke="currentColor" stroke-width="2.2" ' +
      'stroke-linecap="round" opacity=".5"/>' +
      '<path d="M10 24h14c4 0 6-3 6-7s2-7 6-7h8" stroke="currentColor" stroke-width="2.2" ' +
      'stroke-linecap="round" opacity=".5"/>' +
      '<path d="M10 36h8c4 0 6-3 6-7V19c0-4 2-7 6-7h8" stroke="currentColor" stroke-width="2.2" ' +
      'stroke-linecap="round"/>' +
      '<circle cx="38" cy="12" r="4" fill="currentColor"/>'],
    ['4', '六边节点', '守护/承接；与侧栏已有的六边形图标语言一致',
      '<path d="M24 6 L39 15 V33 L24 42 L9 33 V15 Z" stroke="currentColor" stroke-width="2.4" ' +
      'stroke-linejoin="round" opacity=".55"/>' +
      '<circle cx="24" cy="24" r="5.4" fill="currentColor"/>'],
    ['5', '双环交错', '两域/多账号交叉，最简洁的「多」意象',
      '<circle cx="18.5" cy="24" r="10.5" stroke="currentColor" stroke-width="2.6" opacity=".85"/>' +
      '<circle cx="29.5" cy="24" r="10.5" stroke="currentColor" stroke-width="2.6" opacity=".85"/>'],
    ['6', '盾 · 闸', '网关守卫意象，带勾表示可用性',
      '<path d="M24 6 L40 13 V25c0 9-6.5 15.5-16 18-9.5-2.5-16-9-16-18V13z" ' +
      'fill="currentColor" opacity=".18"/>' +
      '<path d="M24 6 L40 13 V25c0 9-6.5 15.5-16 18-9.5-2.5-16-9-16-18V13z" ' +
      'stroke="currentColor" stroke-width="2.4" stroke-linejoin="round"/>' +
      '<path d="M17 24.5l5 5 9.5-10" stroke="currentColor" stroke-width="2.8" ' +
      'stroke-linecap="round" stroke-linejoin="round"/>']
  ];

  function svg(inner, cls) {
    return '<svg viewBox="0 0 48 48" fill="none" aria-hidden="true"' +
      (cls ? ' class="' + cls + '"' : '') + '>' + inner + '</svg>';
  }
  function esc(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // 卡片列表
  var cards = document.getElementById('cards');
  cards.innerHTML = L.map(function (it) {
    return '<div class="card">' +
      '<div class="ico">' + svg(it[3]) + '</div>' +
      '<div class="meta"><div class="nm">' + esc(it[0]) + '. ' + esc(it[1]) + '</div>' +
      '<div class="ds">' + esc(it[2]) + '</div></div>' +
      '<div class="sizes">' +
        '<span class="s28">' + svg(it[3]) + '</span>' +
        '<span class="s20">' + svg(it[3]) + '</span>' +
        '<span class="s16">' + svg(it[3]) + '</span>' +
      '</div></div>';
  }).join('');

  // 顶栏预览（用 1 号）
  document.getElementById('bbIc').innerHTML = svg(L[0][3]);

  // 并排
  document.getElementById('strip').innerHTML =
    L.map(function (it) { return svg(it[3]); }).join('');

  // favicon 尺寸
  document.getElementById('favrow').innerHTML =
    L.map(function (it) {
      return '<div class="fav"><div class="box">' + svg(it[3]) + '</div>' +
        '<div class="lb">' + esc(it[0]) + '</div></div>';
    }).join('');

  // 明暗切换（记住选择）
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem('logoTheme');
    if (saved) root.dataset.theme = saved;
  } catch (e) { /* 隐私模式：忽略 */ }
  document.getElementById('btnT').onclick = function () {
    root.dataset.theme = root.dataset.theme === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem('logoTheme', root.dataset.theme); } catch (e) { /* 忽略 */ }
  };
}());
