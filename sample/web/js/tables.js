/* 两张表（数据列表 / 上传记录）+ 汇总条 + 分页。draw() 是唯一入口。
   加列：在对应的 *_COLS 里加一项（表头 + 取值函数）即可，不用改别处。 */
const pathCell = v => `<td class="cp" data-v="${esc(v)}" title="${esc(v)}（点击复制）">${esc(v)}</td>`;
const remoteTag = v => !v ? '<span class="muted">—</span>' : tag(v.status + (v.deep ? '·深' : ''), cls(v.status),
  v.error || `本地${v.local_files} 远端${v.remote_files} 缺${(v.missing || []).length} 多${(v.extra || []).length} 大小差${(v.size_diff || []).length} 哈希差${(v.hash_diff || []).length}｜${v.checked_at}`);

/* 「房间」单元格：点击在本机文件管理器里打开对应的 PRT 文件夹。
   颜色：橙色 = 不是 PRT_WX_room002（3 位）格式；紫色 = OSS 路径与本地目录名不同（被界面覆盖/补零）。 */
function roomCell(room, id, rel, bad, diff) {
  const cls_ = bad ? 'rm-bad' : diff ? 'rm-diff' : '';
  const tip = [bad, diff, '点击打开本地文件夹（改名后请重新扫描）'].filter(Boolean).join('；');
  return `<td class="room ${cls_}" data-act="openFolder" data-id="${esc(id)}" data-rel="${esc(rel)}" title="${esc(tip)}">${esc(room)}</td>`;
}

/* 「会话目录」单元格：显示 ISuit-V3_… / UMI_v3_… 这一层，点击在本机文件管理器里打开该会话目录；悬浮显示本地全路径 */
const sessCell = r => isUmi(r)      // UMI_v3：点会话名预览视频（看二维码判断标定），📂 打开文件夹；ISuit-V3：点击直接打开文件夹
  ? `<td class="room vlink" data-act="openVideo" data-id="${esc(r.id)}" title="点击预览视频（画面里有二维码 = 标定）｜${esc(r.source)}">▶ ${esc(r.name)}<span class="fold" data-act="openFolder" data-id="${esc(r.id)}" data-rel="${esc(r.rel)}" data-target="session" title="打开文件夹">📂</span></td>`
  : `<td class="room" data-act="openFolder" data-id="${esc(r.id)}" data-rel="${esc(r.rel)}" data-target="session" title="${esc(r.source)}（点击打开文件夹）">${esc(r.name)}</td>`;

/* 数据列表的列：[表头, 排序字段(可空), 是否右对齐, 单元格函数(r) → <td>…] */
const DATA_COLS = [
  ['格式', 'fmt', 0, r => `<td>${fmtTag(r.fmt)}</td>`],
  ['类型', 'type', 0, r => `<td title="${esc(r.rel)}">${r.type === 'normal' ? '常规' : '标定'}${r.misplaced ? ' ' + tag('错放', 'bad', r.misplaced) : ''}${shortCal(r) ? ' ' + tag('<3分钟', 'wn', '时长不足 3 分钟，UMI_v3 的标定会话通常很短（仅提示）。点会话名看视频：画面里有二维码 = 标定') : ''}</td>`],
  ['设备', 'device', 0, r => `<td class="${r.dev_diff ? 'rm-bad' : ''}" title="${esc(r.dev_diff || (isUmi(r) ? '设备文件夹名，仅作参考（UMI_v3 的设备与设备SN 没有对应关系）' : '设备文件夹名（应为设备SN 后 4 位）'))}">${esc(r.device)}</td>`],
  ['设备SN', 'device_sn', 0, r => `<td>${esc(r.device_sn)}</td>`],
  ['房间', 'room', 0, r => roomCell(r.room, r.id, r.rel, r.room_bad, r.room_diff)],
  ['会话目录', 'name', 0, sessCell],
  ['目录日期', 'dir_date', 0, r => `<td class="${r.date_warn ? 'dwc' : ''}" title="${esc(r.date_warn)}">${esc(r.dir_date)}</td>`],
  ['采集日期', 'cap_date', 0, r => `<td class="${r.date_warn ? 'dwc' : ''}">${esc(r.cap_date)}</td>`],
  ['任务名', 'task', 0, r => `<td title="${esc(r.source)}">${esc(r.task)}</td>`],
  ['子任务', 'sub_task', 0, r => `<td>${esc(r.sub_task)}</td>`],
  ['操作员', 'operator', 0, r => `<td>${esc(r.operator)}</td>`],
  ['地点', 'location', 0, r => `<td>${esc(r.location)}</td>`],
  ['采集时间', 'start', 0, r => `<td>${esc(r.start)}</td>`],
  ['分片', 'slices', 1, r => `<td class="n">${r.slices}</td>`],
  ['时长', 'duration', 1, r => `<td class="n">${hms(r.duration)}</td>`],
  ['大小', 'size', 1, r => `<td class="n">${sz(r.size)}</td>`],
  ['上传状态', 'up_state', 0, r => `<td>${tag(r.up_state, cls(r.up_state), r.up_at)}</td>`],
  ['远端校验', null, 0, r => `<td>${remoteTag(r.remote)}</td>`],
  ['OSS 路径', null, 0, r => pathCell(r.oss_url)],
];

/* 上传记录的列（数据来自 /api/uploads，按「采集时间」倒序；采集日期取自 rel，目录日期≠采集日期的行整行标黄）：[表头, 是否右对齐, 单元格函数(h, 日期不一致, 采集日期)] */
const REC_COLS = [
  ['采集时间', 0, h => `<td title="登记时间 ${esc(h.created_at)}">${esc(h.start_time)}</td>`], ['状态', 0, h => `<td>${tag(h.status, cls(h.status), h.error)}</td>`],
  ['类型', 0, h => `<td>${h.type === 'normal' ? '常规' : '标定'}</td>`], ['格式', 0, h => `<td>${fmtTag(h.fmt)}</td>`], ['设备', 0, h => `<td>${esc(h.device)}</td>`], ['设备SN', 0, h => `<td>${esc(h.device_sn)}</td>`],
  ['房间', 0, h => roomCell(h.room, '', h.rel, roomBad(h.room) ? '房间不是 3 位格式，应为 PRT_XX_room002' : '', '')],
  ['目录日期', 0, (h, dw) => `<td class="${dw ? 'dwc' : ''}" title="${dw ? '目录日期与 session 日期不一致' : ''}">${esc(h.dir_date)}</td>`],
  ['采集日期', 0, (h, dw, cp) => `<td class="${dw ? 'dwc' : ''}">${cp}</td>`],
  ['任务名', 0, h => `<td title="${esc(h.source)}">${esc(h.task)}</td>`], ['子任务', 0, h => `<td>${esc(h.sub_task)}</td>`], ['操作员', 0, h => `<td>${esc(h.operator)}</td>`], ['地点', 0, h => `<td>${esc(h.location)}</td>`],
  ['采集结束', 0, h => `<td>${esc(h.end_time)}</td>`],
  ['分片', 1, h => `<td class="n">${h.slices}</td>`], ['采集时长', 1, h => `<td class="n">${hms(h.duration)}</td>`], ['大小', 1, h => `<td class="n">${sz(h.size)}</td>`],
  ['文件数', 1, h => `<td class="n">${h.files}</td>`], ['方式', 0, h => `<td>${esc(h.method)}</td>`], ['耗时(s)', 1, h => `<td class="n">${h.elapsed}</td>`],
  ['平均速度', 1, h => `<td class="n">${h.avg_speed ? sz(h.avg_speed) + '/s' : '—'}</td>`], ['登记时间', 0, h => `<td class="muted">${esc(h.created_at)}</td>`],
  ['OSS 相对路径', 0, h => pathCell(h.rel)], ['OSS 全路径', 0, h => pathCell(h.oss_url)],
  ['manifest.json', 0, h => pathCell(h.oss_url + 'manifest.json')], ['_READY', 0, h => pathCell(h.oss_url + '_READY')],
];

const emptyRow = (n, msg) => `<tbody><tr><td class="empty" colspan="${n}">${msg}</td></tr></tbody>`;

function draw() {
  const ps = +$('ps').value;
  document.querySelectorAll('.act').forEach(b => b.disabled = S.busy || S.scanning);
  $('bup').textContent = '上传选中' + (S.sel.size ? ` (${S.sel.size})` : '');
  $('xlsBtn').hidden = S.view !== 2;                     // 「导出 Excel」只在上传记录页签显示
  renderFmtSeg(); renderMoveBtns();
  S.view === 2 ? drawRecords(ps) : drawDatasets(ps);
}

/* 格式分组按钮（全部 / ISuit-V3 / UMI_v3）：数据列表带各格式的会话数 */
function renderFmtSeg() {
  const c = {'': S.D.length, isuit: S.D.filter(r => !isUmi(r)).length, umi_v3: S.D.filter(isUmi).length}, f = S.F.fmt || '';
  $('fmtSeg').innerHTML = [['', '全部'], ['isuit', S.formats.isuit || 'ISuit-V3'], ['umi_v3', S.formats.umi_v3 || 'UMI_v3']].map(([v, t]) =>
    `<button data-act="setFmt" data-v="${v}" class="${f === v ? 'on' : ''}" title="${v ? `只看 ${t}（上传到桶 ${esc(S.buckets[v] || '')}）` : '两种格式一起显示'}">${t}${S.view === 1 ? ` <small>${c[v]}</small>` : ''}</button>`).join('');
}

/* 标定整理按钮（两种格式各有各的方式）：ISuit-V3 自动判定错放（N=被判定为标定的常规会话）；UMI_v3 手动：移动勾选的会话（先点会话名看视频） */
function renderMoveBtns() {
  const f = S.F.fmt || '', hasI = S.D.some(r => !isUmi(r)) && f !== 'umi_v3', hasU = S.D.some(isUmi) && f !== 'isuit', ns = S.D.filter(r => S.sel.has(r.id));
  const nAuto = S.D.filter(movable).length, uN = ns.filter(r => isUmi(r) && r.type === 'normal').length, uC = ns.filter(r => isUmi(r) && r.type === 'calibration').length;
  $('mvGrp').hidden = S.view !== 1 || !(hasI || hasU); $('mvBtn').hidden = !hasI; $('umvBtn').hidden = !hasU; $('mvBackBtn').hidden = !hasU;
  $('mvBtn').textContent = 'ISuit 自动' + (nAuto ? ` (${nAuto})` : ''); $('umvBtn').textContent = 'UMI 移到标定' + (uN ? ` (${uN})` : ''); $('mvBackBtn').textContent = 'UMI 移回常规' + (uC ? ` (${uC})` : '');
}

function drawRecords(ps) {
  const RS = S.RS, pages = Math.max(1, Math.ceil(RS.total / ps));
  $('pgi').textContent = `${S.page + 1} / ${pages} 页 · 共 ${RS.total} 条`;
  $('cards').innerHTML = `查询结果：<b>${RS.total}</b> 条记录 · <b>${RS.datasets || 0}</b> 个数据集 · 累计采集时长 <b>${hms(RS.duration)}</b> · 体积 <b>${sz(RS.size)}</b> <span class="muted">｜ 已上传 ${RS.ok_datasets || 0} 个 · ${hms(RS.ok_duration)} · ${sz(RS.ok_size)}</span>`;
  const RC = REC_COLS;
  $('tb').innerHTML = '<thead><tr>' + RC.map(c => `<th class="${c[1] ? 'n' : ''}">${c[0]}</th>`).join('') + '</tr></thead>' +
    (S.RH.length ? '<tbody>' + S.RH.map(h => {
      const cp = h.cap_date || capOf(h.rel), dw = dwOf(cp, h.dir_date);
      return `<tr class="${dw ? 'dw' : ''}">${RC.map(c => c[2](h, dw, cp)).join('')}</tr>`;
    }).join('') + '</tbody>' : emptyRow(RC.length, '没有符合条件的上传记录'));
}

function drawDatasets(ps) {
  S.R = rows();
  const pages = Math.max(1, Math.ceil(S.R.length / ps)); S.page = Math.min(S.page, pages - 1);
  const page = S.R.slice(S.page * ps, (S.page + 1) * ps);
  $('pgi').textContent = `${S.page + 1} / ${pages} 页 · 共 ${S.R.length} 条`;
  const tot = S.R.reduce((a, r) => [a[0] + (r.duration || 0), a[1] + (r.size || 0)], [0, 0]);
  $('cards').innerHTML = `查询结果：<b>${S.R.length}</b> 个会话 · 累计采集时长 <b>${hms(tot[0])}</b> · 体积 <b>${sz(tot[1])}</b> <span class="muted">｜ 日期不一致 ${S.R.filter(r => r.date_warn).length} · 房间格式 ${S.R.filter(r => r.room_bad).length} ·${S.R.some(isUmi) ? ` UMI 时长&lt;3分钟 ${S.R.filter(shortCal).length} ·` : ''}${S.R.some(r => !isUmi(r)) ? ` ISuit 错放 ${S.R.filter(r => r.misplaced).length} ·` : ''} 设备≠SN ${S.R.filter(r => r.dev_diff).length} · 已上传 ${S.R.filter(r => r.up_state === '已上传').length} · 已选 ${S.sel.size}</span>`;
  const th = c => `<th class="${c[2] ? 'n' : ''}" ${c[1] ? `data-act="sort" data-k="${c[1]}"` : ''}>${c[0]}${S.sortK === c[1] ? (S.sortD > 0 ? ' ▲' : ' ▼') : ''}</th>`;
  const DC = DATA_COLS;
  $('tb').innerHTML = `<thead><tr><th><input type="checkbox" id="all" title="全选当前筛选结果（含所有页）"></th>${DC.map(th).join('')}</tr></thead>` +
    (page.length ? '<tbody>' + page.map(r => `<tr class="${S.sel.has(r.id) ? 'sel' : ''}${rowWarn(r) ? ' dw' : ''}"><td><input type="checkbox" data-id="${r.id}" ${S.sel.has(r.id) ? 'checked' : ''}></td>${DC.map(c => c[3](r)).join('')}</tr>`).join('') + '</tbody>'
      : emptyRow(DC.length + 1, S.D.length ? '没有符合条件的数据' : '还没有数据：先「＋ 添加目录」选择盘符/目录，再点「扫描」'));
  const all = $('all'); if (all) all.checked = S.R.length > 0 && S.R.every(r => S.sel.has(r.id));
}

/* 勾选：表头复选框 = 全选当前筛选结果（含所有页） */
function onTableChange(e) {
  const t = e.target;
  if (t.id === 'all') { S.R.forEach(r => t.checked ? S.sel.add(r.id) : S.sel.delete(r.id)); draw(); return; }
  if (t.dataset.id) { t.checked ? S.sel.add(t.dataset.id) : S.sel.delete(t.dataset.id); t.closest('tr').classList.toggle('sel', t.checked); $('bup').textContent = '上传选中' + (S.sel.size ? ` (${S.sel.size})` : ''); renderMoveBtns(); }
}
function onTableClick(e) {                                 // 路径单元格点击复制
  const c = e.target.closest('td.cp');
  if (c && navigator.clipboard) navigator.clipboard.writeText(c.dataset.v).then(() => toast('已复制'));
}
