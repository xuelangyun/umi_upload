/* 过滤条：「暂存—查询」模式。控件里改的是暂存值，点「查询」/回车才写入 S.F 并生效；切换页签沿用已生效条件。
   数据列表在前端过滤（rows），上传记录在后端 SQL 过滤（loadRecords → /api/uploads）。两边字段含义相同。 */
const STATUS_OPTIONS = {
  1: [['', '全部状态'], ['未上传', '未上传'], ['已上传', '已上传'], ['上传失败', '上传失败'], ['远端缺失', '远端缺失'], ['mis', '仅错放'], ['warn', '有提示（日期/房间/设备）']],
  2: [['', '全部状态'], ['ok', '已上传(成功+已存在)'], ['成功', '成功'], ['已存在', '已存在'], ['失败', '失败']],
};

function setStatusOptions() {
  const v = $('fs').value, o = STATUS_OPTIONS[S.view];
  $('fs').innerHTML = o.map(x => `<option value="${x[0]}">${x[1]}</option>`).join('');
  $('fs').value = o.some(x => x[0] === v) ? v : '';
}

const readFilters = () => ({type: $('ft').value, status: $('fs').value, device: $('fd').value, cap_date: $('fe').value, operator: $('fo').value,
  task: $('fk').value.trim(), location: $('fc').value.trim(), q: $('q').value.trim()});

function query() { S.F = Object.assign(readFilters(), {fmt: S.F.fmt || ''}); S.page = 0; refresh(); }      // fmt（格式分组按钮）不在下拉里，查询时保留
function clearFilters() { for (const id of ['ft', 'fs', 'fd', 'fe', 'fo', 'fk', 'fc', 'q']) $(id).value = ''; S.F = {fmt: ''}; query(); }
function setFmt(el) { S.F = Object.assign({}, S.F, {fmt: el.dataset.v}); S.page = 0; refresh(); }                  // 格式分组：全部 / ISuit-V3 / UMI_v3
function refresh() { S.view === 1 ? draw() : loadRecords(); }          // 重新取/画当前页签

/* 下拉选项只在内容变化时重建（避免轮询时闪烁、打断用户正在打开的下拉） */
function setOptions(id, vals, allLabel) {
  const u = [...new Set(vals)].filter(Boolean).sort(), key = S.view + u.join('\u0001');
  if (S.sig[id] === key) return;
  S.sig[id] = key;
  const sel = $(id), v = sel.value;
  sel.innerHTML = `<option value="">${allLabel}</option>` + u.map(x => `<option>${esc(x)}</option>`).join('');
  sel.value = u.includes(v) ? v : '';
}
function fillOptions() {
  const f = S.view === 1 ? {device: S.D.map(r => r.device), cap_date: S.D.map(r => r.cap_date), operator: S.D.map(r => r.operator)} : S.facets;
  setOptions('fd', f.device || [], '全部设备'); setOptions('fe', f.cap_date || [], '全部采集日期'); setOptions('fo', f.operator || [], '全部人员');
}

async function loadRecords() {
  $('wr').classList.add('ld');
  try {
    const q = new URLSearchParams(Object.assign({}, S.F, {ps: $('ps').value, page: S.page}));
    S.RS = await (await fetch('/api/uploads?' + q)).json(); S.RH = S.RS.rows; S.facets = S.RS.facets;
    fillOptions(); draw();
  } finally { $('wr').classList.remove('ld'); }
}

/* 数据列表：过滤 + 排序（全部行，分页在 draw 里切） */
const hit = (r, Q) => { const h = lcs([r.name, r.source, r.task, r.sub_task, r.operator, r.rel, r.location, r.device, r.device_sn].join(' ')).replace(/\\/g, '/'); return Q.every(w => h.includes(w)); };   // 关键字：每个词都出现在（会话目录/本地路径/任务/人员/地点/设备/SN）里
const lcs = s => (s || '').toLowerCase();
function datasetOverview() {
  const key = S.datasetETag || `v:${S.datasetVersion || 0}:${S.D.length}`;
  if (S.datasetOverviewCache?.key === key) return S.datasetOverviewCache;
  const stats = {total: S.D.length, isuit: 0, umi_v3: 0, warn: 0, misplaced: 0};
  const byId = new Map();
  for (const r of S.D) {
    byId.set(r.id, r);
    if (isUmi(r)) stats.umi_v3++;
    else { stats.isuit++; if (movable(r)) stats.misplaced++; }
    if (hasWarn(r)) stats.warn++;
  }
  const result = {key, stats, byId};
  S.datasetOverviewCache = result;
  return result;
}

function rows() {
  const f = S.F, lc = s => (s || '').toLowerCase(), T = lc(f.task), L = lc(f.location), Q = lc(f.q).replace(/\\/g, '/').split(/\s+/).filter(Boolean);
  const snapshotKey = S.datasetETag || `v:${S.datasetVersion || 0}:${S.D.length}`;
  const key = JSON.stringify([snapshotKey, f.fmt || '', f.type || '', f.device || '', f.cap_date || '', f.operator || '', T, L, f.status || '', Q, S.sortK, S.sortD]);
  if (S.datasetViewCache?.key === key) {
    S.R = S.datasetViewCache.rows;
    return S.R;
  }
  const result = S.D.filter(r => (!f.fmt || r.fmt === f.fmt) && (!f.type || r.type === f.type) && (!f.device || r.device === f.device) && (!f.cap_date || r.cap_date === f.cap_date)
    && (!f.operator || r.operator === f.operator) && (!T || lc(r.task).includes(T)) && (!L || lc(r.location).includes(L))
    && (!f.status || (f.status === 'mis' ? r.misplaced : f.status === 'warn' ? hasWarn(r) : r.up_state === f.status))
    && (!Q.length || hit(r, Q)))
    .sort((a, b) => { const x = a[S.sortK], y = b[S.sortK]; return (x > y ? 1 : x < y ? -1 : 0) * S.sortD || (a.rel < b.rel ? -1 : a.rel > b.rel ? 1 : 0); });
  S.R = result;
  S.datasetViewCache = {key, rows: result};
  S.datasetFilteredStatsCache = null;
  return result;
}
