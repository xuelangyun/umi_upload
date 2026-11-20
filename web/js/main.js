/* 入口：事件委托（data-act / data-change）+ 轮询 + 页签/扫描/任务的主流程。
   新增一个按钮：HTML 里写 data-act="名字"，在下面 ACT 表里加对应函数。 */
const ACT = {
  cfg: el => setCfg({[el.dataset.k]: el.dataset.v}),
  openConfig, saveConfig: el => saveConfig(!!el.dataset.test), openBrowse, addRoots, openProblems, openGroups, shutdown, applyNet, setFmt, openFolder, openMove, doMove, openVideo, stepVideo, closeVideo, videoMove, videoFolder,
  closeModal: el => closeModal(el.dataset.m),
  enter: el => browse(el.dataset.p),
  rmRoot: el => removeRoot(el.dataset.r),
  exportXlsx: () => { location.href = '/api/uploads/export?' + new URLSearchParams(S.F); },     // 用已生效的查询条件导出（浏览器下载）
  scan, run: el => run(el.dataset.a), tab: el => tab(+el.dataset.n), query, clear: clearFilters,
  page: el => { S.page = Math.max(0, S.page + +el.dataset.d); refresh(); },
  sort: el => { S.sortK = el.dataset.k; S.sortD *= -1; draw(); },
  toggleLog: el => { const f = $('logbox').classList.toggle('fold'); el.textContent = f ? '展开 ▴' : '折叠 ▾'; },
  ackNotice: async () => { await api('/api/notice/ack', {}); poll(); },
};
const CHANGE = {
  cfg: el => setCfg({[el.dataset.k]: el.value}),
  pagesize: () => { S.page = 0; refresh(); },
};

document.addEventListener('click', e => { const el = e.target.closest('[data-act]'); if (el && ACT[el.dataset.act]) ACT[el.dataset.act](el, e); });
document.addEventListener('change', e => { const el = e.target.closest('[data-change]'); if (el && CHANGE[el.dataset.change]) CHANGE[el.dataset.change](el, e); });
$('flt').addEventListener('keydown', e => { if (e.key === 'Enter') query(); });
$('tb').addEventListener('change', onTableChange);
$('mbody').addEventListener('change', e => { if (e.target.id === 'mvall') $('mbody').querySelectorAll('input[data-mv]:not(:disabled)').forEach(c => c.checked = e.target.checked); });
$('tb').addEventListener('click', onTableClick);
$('onlye').addEventListener('change', () => S.lastLog = '');
document.querySelectorAll('[data-input=prt]').forEach(e => e.addEventListener('input', prt));

/* ---- 主流程 ---- */
function tab(n) {
  if (n === S.view) return;
  S.view = n; S.page = 0;
  $('t1').classList.toggle('on', n === 1); $('t2').classList.toggle('on', n === 2);
  setStatusOptions(); fillOptions(); query();            // 沿用已暂存的筛选值，不清空表格，避免抖动
}

async function scan() {
  if (S.scanning) return;                                // 防重复点击（服务端也有保护）
  S.scanning = true; setScanUi({running: true, stage: '提交', done: 0, total: 0, elapsed: 0}, 0);
  const r = await api('/api/scan', {city: $('city').value, room: $('room').value, full: $('fullscan').checked, pad: $('padroom').checked});
  if (r.ok) S.sel.clear();
  poll();
}

async function run(action) {
  if (S.scanning) return toast('正在扫描，请稍候');
  if (!S.sel.size) return toast('请先勾选数据（表头复选框 = 全选当前筛选结果）');
  if (action === 'upload') {
    const sel = S.D.filter(r => S.sel.has(r.id)), total = sel.reduce((x, r) => x + r.size, 0), W = sel.filter(hasWarn);
    const warn = W.length ? `\n\n⚠ 其中 ${W.length} 个有提示（${W.filter(r => r.date_warn).length} 个 session 日期 ≠ 目录日期）：\n` + W.slice(0, 3).map(r => r.name + '：' + [r.date_warn, r.room_bad, r.room_diff, r.dev_diff].filter(Boolean).join('；')).join('\n') + (W.length > 3 ? '\n…' : '') + '\n上传路径里的日期取自 session 目录名。' : '';
    const ordMap = {folder: 'Go SDK·逐文件夹', slice: 'Go SDK·逐分片', pipeline: 'Go SDK·流水线'};
    const mode = ordMap[document.querySelector('[data-k=order].on')?.dataset.v] || 'Go SDK·逐文件夹';
    const by = {}; sel.forEach(r => by[r.fmt] = (by[r.fmt] || 0) + 1);        // 两种格式各传各的桶
    const dest = Object.keys(by).map(f => `${S.formats[f] || f} ${by[f]} 个 → 桶 ${S.buckets[f]}`).join('；');
    if (!confirm(`上传 ${sel.length} 个数据集，约 ${sz(total)}？\n${dest}\n方式：${mode}，数据集并发：${$('dsj').value}` + warn)) return;
  }
  const r = await api('/api/run', {action, ids: [...S.sel], force: $('force').checked});
  if (r.ok) toast('任务已开始');
  poll();
}

/* 轮询：任务/扫描进行中由定时器每 0.6 秒调用 poll(true) 走轻量接口（只取进度，不带数据集列表）；
   用户操作、任务/扫描刚结束、或距上次完整刷新超过 5 秒时才取完整状态（数据集很多时完整状态有几十 MB）。 */
async function poll(tick) {
  clearTimeout(S.timer);
  if (S.stopped) return;
  const was = S.busy, light = tick === true && S.loaded && Date.now() - S.lastFull < 5000;
  const s = await api('/api/state' + (light ? '?light=1' : ''));
  if (!s.scan) return;
  if (light) {
    const busyNow = !!(s.job && !s.job.done);
    if ((was && !busyNow) || (S.prevScan && !s.scan.running)) return poll();       // 刚结束：立刻取完整状态（刷新行状态/上传记录/扫描结果）
    renderControls(s); renderBuckets(s); setScanUi(s.scan, s.roots.length); showJob(s.job);
    S.timer = setTimeout(() => poll(true), 600); return;
  }
  S.loaded = true; S.lastFull = Date.now();
  S.D = s.datasets; S.PR = s.problems; S.busy = !!(s.job && !s.job.done);
  const have = new Set(S.D.map(r => r.id)); S.sel.forEach(id => have.has(id) || S.sel.delete(id));      // 数据集被清掉（如改桶换了格式）后，去掉失效的勾选
  renderControls(s); renderBuckets(s); renderNotice(s.notice); renderRoots(s.roots);
  const nw = renderProblemsButton(); setScanUi(s.scan, s.roots.length);
  $('sinfo').title = '数据库：' + s.db;
  if (S.prevScan && !s.scan.running) {                   // 扫描刚结束：反馈结果
    const rs = s.scan.result || {};
    toast(rs.error ? '扫描失败：' + rs.error : `扫描完成：${rs.count} 个会话（重新统计 ${rs.scanned} · 缓存 ${rs.cached}），${rs.problems} 个问题` + (nw ? `，${nw} 个有日期/房间提示` : '') + `，用时 ${s.scan.elapsed}s`);
    if (!rs.error && (S.PR.length || nw)) openProblems();
  }
  S.prevScan = s.scan.running;
  if (!$('fs').options.length) setStatusOptions();
  if (S.view === 1) fillOptions();
  if (!s.roots.length) $('pl').textContent = '先点「＋ 添加目录」（可一次勾选多个盘符），再点「扫描」';
  showJob(s.job);
  (S.view === 2 && was && !S.busy) ? loadRecords() : draw();    // 任务刚结束时刷新上传记录
  if (S.busy || s.scan.running) S.timer = setTimeout(() => poll(true), 600);
}

prt(); poll();
