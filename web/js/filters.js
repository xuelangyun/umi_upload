/* 过滤条：「暂存—查询」模式。控件里改的是暂存值，点「查询」/回车才写入 S.F 并生效；切换页签沿用已生效条件。
   数据列表在前端过滤（rows），上传记录在后端 SQL 过滤（loadRecords → /api/uploads）。两边字段含义相同。 */
/* Shared dataset overview for filters, warning badges, and format controls. */
function datasetOverview() {
  const key = S.datasetETag || `v:${S.datasetVersion || 0}:${S.D.length}`;
  const cached = S.datasetOverviewCache;
  if (cached?.key === key && cached.data === S.D) return cached;
  S.selectionStats = null;

  const stats = {total: S.D.length, isuit: 0, umi_v3: 0, warn: 0, misplaced: 0};
  const byId = new Map();
  const optionSets = {device: new Set(), cap_date: new Set(), operator: new Set()};
  for (const row of S.D) {
    byId.set(row.id, row);
    if (isUmi(row)) stats.umi_v3++;
    else stats.isuit++;
    if (movable(row)) stats.misplaced++;
    if (hasWarn(row)) stats.warn++;
    if (row.device) optionSets.device.add(row.device);
    if (row.cap_date) optionSets.cap_date.add(row.cap_date);
    if (row.operator) optionSets.operator.add(row.operator);
  }

  const facets = {}, facetKeys = {};
  for (const key of Object.keys(optionSets)) {
    facets[key] = [...optionSets[key]].sort();
    facetKeys[key] = facets[key].join(String.fromCharCode(1));
  }
  const overview = {key, data: S.D, stats, byId, facets, facetKeys};
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
function setOptions(id, vals, allLabel, preparedKey) {
  const u = preparedKey === undefined ? [...new Set(vals)].filter(Boolean).sort() : vals;
  const key = S.view + (preparedKey === undefined ? u.join(String.fromCharCode(1)) : preparedKey);
  if (S.sig[id] === key) return;
  S.sig[id] = key;
  const sel = $(id), v = sel.value;
  sel.innerHTML = `<option value="">${allLabel}</option>` + u.map(x => `<option>${esc(x)}</option>`).join('');
  sel.value = u.includes(v) ? v : '';
}
function fillOptions() {
  if (S.view === 1) {
    const {facets, facetKeys} = datasetOverview();
    setOptions('fd', facets.device, '全部设备', facetKeys.device);
    setOptions('fe', facets.cap_date, '全部采集日期', facetKeys.cap_date);
    setOptions('fo', facets.operator, '全部人员', facetKeys.operator);
    return;
  }
  const f = S.facets;
  setOptions('fd', f.device || [], '全部设备');
  setOptions('fe', f.cap_date || [], '全部采集日期');
  setOptions('fo', f.operator || [], '全部人员');
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

/* 数据列表：仅在数据版本、筛选条件或排序字段变化时重新过滤/排序。分页和重绘复用结果。 */
const lcs = value => String(value ?? '').toLowerCase();
function rows() {
  const f = S.F || {};
  const datasetKey = S.datasetETag || ('v:' + (S.datasetVersion || 0) + ':' + S.D.length);
  const filterKey = JSON.stringify([
    datasetKey, f.fmt || '', f.type || '', f.device || '', f.cap_date || '',
    f.operator || '', f.task || '', f.location || '', f.status || '', f.q || ''
  ]);
  const sortKey = filterKey + '\u0000' + S.sortK + '\u0000' + S.sortD;
  if (S.datasetViewCache?.key === sortKey) return S.datasetViewCache.rows;

  const task = lcs(f.task), location = lcs(f.location);
  const words = lcs(f.q).replace(/\\/g, '/').split(/\s+/).filter(Boolean);
  const result = [];
  for (const row of S.D) {
    if (f.fmt && row.fmt !== f.fmt) continue;
    if (f.type && row.type !== f.type) continue;
    if (f.device && row.device !== f.device) continue;
    if (f.cap_date && row.cap_date !== f.cap_date) continue;
    if (f.operator && row.operator !== f.operator) continue;
    if (task && !lcs(row.task).includes(task)) continue;
    if (location && !lcs(row.location).includes(location)) continue;
    if (f.status && !(f.status === 'mis' ? row.misplaced : f.status === 'warn' ? hasWarn(row) : row.up_state === f.status)) continue;
    if (words.length) {
      const searchable = lcs([row.name, row.source, row.task, row.sub_task, row.operator, row.rel, row.location, row.device, row.device_sn].join(' ')).replace(/\\/g, '/');
      if (!words.every(word => searchable.includes(word))) continue;
    }
    result.push(row);
  }
  result.sort((a, b) => {
    const x = a[S.sortK], y = b[S.sortK];
    return (x > y ? 1 : x < y ? -1 : 0) * S.sortD || (a.rel < b.rel ? -1 : a.rel > b.rel ? 1 : 0);
  });
  S.datasetViewCache = {key: sortKey, filterKey, rows: result};
  return result;
}
