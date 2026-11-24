/* 过滤条：「暂存—查询」模式。控件里改的是暂存值，点「查询」/回车才写入 S.F 并生效；切换页签沿用已生效条件。
   数据列表在前端过滤（rows），上传记录在后端 SQL 过滤（loadRecords → /api/uploads）。两边字段含义相同。 */
/* Shared dataset overview for filters, warning badges, and format controls. */
function datasetOverview() {
  const key = S.datasetETag || `v:${S.datasetVersion || 0}:${S.D.length}`;
  if (S.datasetOverviewCache?.key === key) return S.datasetOverviewCache;

  const stats = {total: S.D.length, isuit: 0, umi_v3: 0, warn: 0, misplaced: 0};
  const byId = new Map();
  for (const row of S.D) {
    byId.set(row.id, row);
    if (isUmi(row)) stats.umi_v3++;
    else stats.isuit++;
    if (movable(row)) stats.misplaced++;
    if (hasWarn(row)) stats.warn++;
  }

  const overview = {key, stats, byId};
  S.datasetOverviewCache = overview;
  return overview;
}

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

let recordsController = null;
let recordsRequestId = 0;

function cancelRecordsRequest() {
  recordsRequestId++;
  recordsController?.abort();
  recordsController = null;
  $('wr').classList.remove('ld');
}

async function loadRecords() {
  recordsController?.abort();
  const controller = new AbortController();
  const requestId = ++recordsRequestId;
  recordsController = controller;
  $('wr').classList.add('ld');

  try {
    const query = new URLSearchParams({...S.F, ps: $('ps').value, page: S.page});
    const result = await requestJSON('/api/uploads?' + query, {signal: controller.signal});
    if (requestId !== recordsRequestId) return;
    if (!Array.isArray(result.rows)) throw new Error('上传记录响应格式不正确');
    S.RS = result;
    S.RH = result.rows;
    S.facets = result.facets || {};
    fillOptions();
    draw();
  } catch (error) {
    if (error.name !== 'AbortError' && requestId === recordsRequestId) toast(error.message || '读取上传记录失败');
  } finally {
    if (requestId === recordsRequestId) {
      $('wr').classList.remove('ld');
      recordsController = null;
    }
  }
}
