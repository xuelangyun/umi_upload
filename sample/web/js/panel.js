/* 页面上方的各个面板：应用栏控件、中断提示、数据源卡片、进度卡片、日志。只负责「把状态画出来」。 */
const CFG_KEY = {method: 'method', order: 'order', ossutil_unit: 'unit', ds_jobs: 'ds_jobs'};   // 配置项 → /api/state 里的字段

function renderControls(s) {
  document.querySelectorAll('[data-act=cfg]').forEach(b => b.classList.toggle('on', String(s[CFG_KEY[b.dataset.k]]) === b.dataset.v));
  document.querySelectorAll('[data-show]').forEach(e => e.hidden = e.dataset.show !== s.method);      // 方式相关的控件
  $('dsj').value = s.ds_jobs;
}

/* 数据源卡片上的「格式 → 桶」：目录名决定会话格式，格式决定上传到哪个桶（两种格式可同时使用） */
function renderBuckets(s) {
  S.buckets = s.buckets || {}; S.formats = s.formats || {};
  $('bkInfo').innerHTML = Object.keys(S.buckets).map(f => `<span class="tag f-${f === 'umi_v3' ? 'umi' : 'isuit'}" title="${esc(S.formats[f])} 格式的数据上传到这个桶；在「配置」里修改">${esc(S.formats[f] || f)} → ${esc(S.buckets[f])}</span>`).join('');
}

function renderNotice(n) {
  const e = $('notice');
  if (!n) { e.hidden = true; return; }
  e.hidden = false;
  e.innerHTML = `<span>⚠ 上次${{upload: '上传', verify: '快速校验', verify_deep: '深度校验'}[n.action] || '任务'}被中断（${esc(n.started_at)}，已完成 ${n.ok}/${n.total}）。已传完的数据不受影响，重新「上传选中」会跳过已完成的、未完成的从断点继续。</span><span class="grow"></span><button class="btn small" data-act="ackNotice">知道了</button>`;
}

function renderRoots(roots) {
  $('roots').innerHTML = roots.map(r => `<span class="chip" title="${esc(r)}">${esc(r)}<i data-act="rmRoot" data-r="${esc(r)}">✕</i></span>`).join('');
}

/* 城市+房间 → 上传路径里的 PRT 目录预览 */
function prt() {
  const c = ($('city').value || '').toUpperCase().replace(/[^A-Z]/g, ''), r = ($('room').value || '').replace(/\D/g, '');
  const e = $('prtp'); e.classList.toggle('ov', !!(c && r));
  e.textContent = c && r ? '覆盖本地 → PRT_' + c + '_room' + r : '按本地目录名';
}

/* 扫描中的按钮/提示状态（防重复点击） */
function setScanUi(sc, nRoots) {
  const r = sc.running; S.scanning = r;
  const b = $('scanBtn'); b.disabled = r || S.busy; b.innerHTML = r ? '<span class="spin"></span> 扫描中…' : '扫描';
  $('addBtn').disabled = r; $('wr').classList.toggle('sc', r);
  $('sinfo').textContent = r ? `扫描中 · ${sc.stage}${sc.total ? ` ${sc.done}/${sc.total}` : ''} · ${sc.elapsed}s` : `${nRoots} 个目录 · ${S.D.length} 个会话`;
}

function renderProblemsButton() {
  const nw = S.D.filter(hasWarn).length, n = S.PR.length + nw, b = $('probBtn');
  b.hidden = !n; b.textContent = '⚠ 提示 ' + n;
  return nw;
}

/* 进度卡片 + 日志。j = /api/state.job */
function showJob(j) {
  if (!j) return;
  const p = j.prog, name = {upload: '上传', verify: '快速校验', verify_deep: '深度校验'}[j.action];
  $('ptitle').textContent = (j.done ? '已完成 · ' : '') + name + ` · 完成 ${p.done_n}/${p.ds_n}` + (j.done ? '' : ` · 进行中 ${p.running}`) + ` · 成功 ${p.ok} 失败 ${p.fail}`;
  $('pct').textContent = p.pct.toFixed(1) + '%'; $('bar1').style.width = p.pct + '%'; $('bar1').parentNode.classList.toggle('run', !j.done);
  $('spd').textContent = j.done ? '—' : (p.rep_speed ? sz(p.rep_speed) : sz(p.speed)) + '/s';
  $('spd').title = p.rep_speed ? 'ossutil 输出的速度（各进程之和）' : '滑动窗口 5 秒实测；ossutil 未输出进度时为上一文件夹的平均速度';
  $('sent').textContent = j.done ? '—' : sz(p.sent) + ' / ' + sz(p.total);
  $('fil').textContent = j.done ? '—' : `${p.fdone}/${p.ftotal} · ${p.files}/${p.total_files}`;
  $('eta').textContent = hms(p.elapsed) + ' / ' + (p.eta != null ? hms(p.eta) : '—');
  $('pl').textContent = j.done ? '已结束 · 平均速度 ' + sz(p.avg) + '/s'
    : `${p.phase}${p.running > 1 ? ` · ${p.running} 个数据集并发` : ''} · 哈希 ${p.total ? (100 * p.hashed / p.total).toFixed(0) : 0}% · ${p.file || p.name}` + (p.raw ? ` ｜ ossutil: ${p.raw}` : '');
  renderLog(j.log);
}

function renderLog(lines) {
  const lg = $('lg'), only = $('onlye').checked, txt = lines.join('\n') + only;
  if (txt === S.lastLog) return;                       // 没变化不重绘（保持滚动位置）
  S.lastLog = txt;
  const atBottom = lg.scrollTop + lg.clientHeight >= lg.scrollHeight - 10;
  lg.innerHTML = lines.filter(l => !only || /✘|错误|失败/.test(l))
    .map(l => `<div class="${/✘|\[错误\]|失败/.test(l) ? 'e' : /✔/.test(l) ? 'o' : /▶ \[/.test(l) ? 'h' : ''}">${esc(l)}</div>`).join('');
  if (atBottom) lg.scrollTop = 1e9;
}
