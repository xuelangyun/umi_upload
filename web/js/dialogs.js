/* 弹窗：扫描提示、目录选择、OSS 配置、退出服务。 */
const openModal = id => $(id).style.display = 'flex', closeModal = id => $(id).style.display = 'none';

/* ---- 扫描提示（扫描问题 + 数据行的日期/房间提示） ---- */
function openProblems() {
  const W = S.D.filter(hasWarn), dw = W.filter(r => r.date_warn).length, ln = x => `<div>${esc(x)}</div>`;
  $('pbody').innerHTML = (S.PR.length ? `<div class="muted">扫描问题 ${S.PR.length} 个</div>` + S.PR.map(ln).join('') : '')
    + (W.length ? `<div class="muted" style="margin-top:8px">数据行提示 ${W.length} 个（其中 session 日期 ≠ 目录日期 ${dw} 个，设备文件夹 ≠ 设备SN 后 4 位 ${W.filter(r => r.dev_diff).length} 个）。上传路径里的日期取自 session 目录名；日期/房间被覆盖的行已标黄，房间格式不规范的「房间」格、与 SN 不符的「设备」格已标橙</div>`
      + W.slice(0, 200).map(r => ln(r.name + '：' + [r.date_warn, r.room_bad, r.room_diff, r.dev_diff].filter(Boolean).join('；'))).join('') + (W.length > 200 ? ln(`…仅列出前 200 条，其余请在数据列表用状态「有提示」筛选查看`) : '') : '');
  openModal('mp');
}

/* ---- 目录选择（浏览服务端文件系统；Windows 空路径列盘符，可多选） ---- */
async function openBrowse() { openModal('mb'); await browse(''); }
let browseController = null;
let browseRequestId = 0;

async function browse(path) {
  browseController?.abort();
  const controller = new AbortController();
  const requestId = ++browseRequestId;
  browseController = controller;

  try {
    const query = new URLSearchParams({path: path || ''});
    const result = await requestJSON('/api/browse?' + query, {signal: controller.signal});
    if (requestId !== browseRequestId) return;
    S.bcur = result.path;
    $('bp').textContent = result.path || '（我的电脑：可勾选多个盘符）';
    $('bh').textContent = result.hint?.length ? '✓ 此目录包含：' + result.hint.join(', ') : '';
    $('bl').innerHTML = (result.parent !== null ? `<div><input type="checkbox" style="visibility:hidden"><span data-act="enter" data-p="${esc(result.parent)}">⬆ 上一级</span></div>` : '')
      + (result.dirs || []).map(dir => `<div><input type="checkbox" data-f="${esc(dir.full)}"><span data-act="enter" data-p="${esc(dir.full)}" title="${esc(dir.full)}">📁 ${esc(dir.name)}</span></div>`).join('');
    $('bl').scrollTop = 0;
  } catch (error) {
    if (error.name !== 'AbortError' && requestId === browseRequestId) toast(error.message || '读取目录失败');
  } finally {
    if (requestId === browseRequestId) browseController = null;
  }
}
async function addRoots() {
  const a = [...$('bl').querySelectorAll('input:checked')].map(e => e.dataset.f);
  if (!a.length && S.bcur) a.push(S.bcur);
  if (!a.length) return toast('请勾选或进入一个目录');
  await api('/api/roots', {add: a}); closeModal('mb'); poll();
}
async function removeRoot(p) {
  if (S.scanning) return toast('正在扫描，暂不能修改目录');
  await api('/api/roots', {remove: [p]}); poll();
}

/* ---- OSS 配置 ---- */
const CFG_FIELDS = ['storage_type', 'endpoint', 'bucket', 'bucket_umi', 'ak', 'sk', 'ds_jobs', 'jobs', 'parallel', 'wan', 'net_wait', 'target_mbps', 'auto_tune', 'max_jobs', 'max_parallel'];
/* 多出口并发：按出口数给建议，并估算当前设置能有多少条并发连接（公式在后端 umi/nettune.py，页面不重复定义） */
async function refreshNet() {
  const q = new URLSearchParams({wan: $('c_wan').value || 1, ds_jobs: $('c_ds_jobs').value || 2, parallel: $('c_parallel').value || 4});
  const j = await api('/api/net/plan?' + q); if (!j.recommend) return;
  S.netRec = j.recommend; const c = j.current, r = j.recommend;
  $('c_net').innerHTML = `当前设置约 <b>${c.conns}</b> 条并发连接，平均每个出口 <b>${c.per_wan}</b> 条 ` + (c.enough ? tag('够用', 'ok') : tag('偏少', 'wn', '每个出口建议 ≥ 24 条并发连接，负载均衡才容易把各出口跑满'))
    + `<br><span class="muted">${j.wan} 个出口的建议：数据集并发 ${r.ds_jobs} · 同时文件数 ${r.jobs} · 分片并发 ${r.parallel}（约 ${r.conns} 条连接）。估算 = 数据集并发 × 每进程约 3 个大文件 × 分片并发</span>`;
}
function applyNet() {
  const r = S.netRec; if (!r) return;
  $('c_ds_jobs').value = r.ds_jobs; $('c_jobs').value = r.jobs; $('c_parallel').value = r.parallel; refreshNet();
  $('cmsg').textContent = '已填入建议值，点「保存」生效'; $('cmsg').style.color = '';
}
['c_wan', 'c_ds_jobs', 'c_parallel'].forEach(id => $(id).addEventListener('input', () => { clearTimeout(S.netT); S.netT = setTimeout(refreshNet, 300); }));
let configLoadController = null;

async function reloadConfigForm() {
  configLoadController?.abort();
  const controller = new AbortController();
  configLoadController = controller;
  try {
    const [config, profiles] = await Promise.all([
      requestJSON('/api/config', {signal: controller.signal}),
      requestJSON('/api/config/profiles', {signal: controller.signal})
    ]);
    if (configLoadController !== controller) return null;

    for (const key of CFG_FIELDS) {
      if (key !== 'sk') $('c_' + key).value = config[key] || '';
    }
    $('c_sk').value = '';
    $('c_sk').placeholder = config.has_sk ? '已保存（留空不修改）' : '请输入';
    $('c_profile_select').innerHTML = (profiles.profiles || []).map(profile =>
      `<option value="${esc(profile.id)}">${esc(profile.name)}${profile.active ? '（当前生效）' : ''}</option>`
    ).join('');
    $('c_profile_select').value = profiles.active_profile_id || '';
    $('cmsg').textContent = '';
    $('cmsg').style.color = '';

    try {
      const bandwidth = await requestJSON('/api/bandwidth', {signal: controller.signal});
      if (configLoadController !== controller) return null;
      $('c_bandwidth_status').textContent = bandwidth.state === 'idle' ? '调优状态：空闲（上传任务开始后运行）'
        : `调优状态：${bandwidth.state} · 当前 ${Number(bandwidth.current_mbps || 0).toFixed(1)} Mbps / 目标 ${bandwidth.target_mbps} Mbps · 文件并发 ${bandwidth.active_files}/${bandwidth.file_limit} · 分片并发 ${bandwidth.parallel} · ${bandwidth.reason || ''}`;
    } catch (error) {
      if (error.name === 'AbortError') return null;
      $('c_bandwidth_status').textContent = '调优状态暂不可用';
    }
    return config;
  } catch (error) {
    if (error.name !== 'AbortError' && configLoadController === controller) toast(error.message || '读取配置失败');
    return null;
  } finally {
    if (configLoadController === controller) configLoadController = null;
  }
}
function closeConfigRequests() {
  configLoadController?.abort();
  browseController?.abort();
}
async function openConfig() {
  const config = await reloadConfigForm();
  if (!config) return;
  openModal('mc'); refreshNet();
}
function configFormValues() {
  const b = {}; for (const k of CFG_FIELDS) b[k] = $('c_' + k).value;
  return b;
}
async function saveConfig(test) {
  const sv = await api('/api/config', configFormValues());
  if (!sv.ok) { $('cmsg').textContent = '未保存：' + (sv.error || '失败'); $('cmsg').style.color = 'var(--bad)'; return; }
  $('cmsg').textContent = '当前配置已保存'; $('cmsg').style.color = '';
  await reloadConfigForm();
  if (test) { $('cmsg').textContent = '测试当前配置…'; const r = await api('/api/config/test', {}); $('cmsg').textContent = r.msg; $('cmsg').style.color = r.ok ? 'var(--ok)' : 'var(--bad)'; }
  poll();
}
async function switchConfigProfile() {
  const id = $('c_profile_select').value;
  if (!id) return toast('没有可切换的配置');
  const r = await api('/api/config/profiles', {action: 'activate', id});
  if (!r.ok) return;
  await reloadConfigForm(); refreshNet(); poll();
  toast('已切换配置');
}
async function saveConfigAsProfile() {
  const name = $('c_profile_name').value.trim();
  if (!name) return toast('请输入新配置名称');
  const r = await api('/api/config/profiles', {action: 'create', name, config: configFormValues()});
  if (!r.ok) return;
  $('c_profile_name').value = '';
  await reloadConfigForm(); refreshNet(); poll();
  toast('已另存为并切换到新配置');
}
async function deleteConfigProfile() {
  const id = $('c_profile_select').value;
  const name = $('c_profile_select').selectedOptions[0]?.textContent || '所选配置';
  if (!id || !confirm(`确定删除配置“${name}”吗？不会删除 OSS 云端数据。`)) return;
  const r = await api('/api/config/profiles', {action: 'delete', id});
  if (!r.ok) return;
  await reloadConfigForm(); refreshNet(); poll();
  toast('配置已删除');
}

/* ---- 退出服务（等同于关闭 cmd 窗口） ---- */
async function shutdown() {
  if (!confirm('退出服务？（等同于关闭 cmd 窗口）')) return;
  const post = body => fetch('/api/shutdown', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
  try {
    let response = await post({});
    if (response.status === 409) {
      const result = await response.json().catch(() => ({}));
      if (!confirm((result.error || '仍有任务在运行') + '\n\n仍要退出吗？')) return;
      response = await post({force: true});
    }
    if (!response.ok) {
      const result = await response.json().catch(() => ({}));
      throw new Error(result.error || `退出失败：HTTP ${response.status}`);
    }
    stopLiveUpdates();
    closeConfigRequests();
    openModal('bye');
  } catch (error) {
    toast(error.message || '退出服务请求失败');
  }
}


/* ---- 累计采集时长明细：按设备 / 按人员（对当前查询结果）。数据列表在前端汇总，上传记录用后端按数据集去重的汇总 ---- */
function groupBy(rows, keyFn, snFn) {
  const m = new Map();
  for (const r of rows) { const k = keyFn(r) || '（空）', g = m.get(k) || {k, sn: snFn ? snFn(r) : '', n: 0, duration: 0, size: 0}; g.n++; g.duration += r.duration || 0; g.size += r.size || 0; m.set(k, g); }
  return [...m.values()].sort((a, b) => b.duration - a.duration);
}
function groupTable(title, items, withSn) {
  const tot = items.reduce((a, g) => [a[0] + g.n, a[1] + g.duration, a[2] + g.size], [0, 0, 0]);
  return `<h4>${title}</h4><table><thead><tr><th>${title.replace('按', '')}</th>${withSn ? '<th>设备SN</th>' : ''}<th class="n">数据集数</th><th class="n">累计采集时长</th><th class="n">占比</th><th class="n">体积</th></tr></thead><tbody>`
    + items.map(g => `<tr><td>${esc(g.k || '（空）')}</td>${withSn ? `<td>${esc(g.sn)}</td>` : ''}<td class="n">${g.n}</td><td class="n">${hms(g.duration)}</td><td class="n">${tot[1] ? (100 * g.duration / tot[1]).toFixed(1) : 0}%</td><td class="n">${sz(g.size)}</td></tr>`).join('')
    + `</tbody><tfoot><tr><td${withSn ? ' colspan="2"' : ''}>合计</td><td class="n">${tot[0]}</td><td class="n">${hms(tot[1])}</td><td class="n">100%</td><td class="n">${sz(tot[2])}</td></tr></tfoot></table>`;
}
function openGroups() {
  let dev, op, scope;
  if (S.view === 2) { dev = S.RS.by_device || []; op = S.RS.by_operator || []; scope = '上传记录的当前查询结果（同一数据集多次上传只算一次）'; }
  else { dev = groupBy(S.R, r => r.device, r => r.device_sn); op = groupBy(S.R, r => r.operator); scope = '数据列表的当前查询结果'; }
  $('gscope').textContent = '范围：' + scope;
  $('gbody').innerHTML = groupTable('按设备', dev, true) + groupTable('按人员', op, false);
  openModal('mg');
}
async function openFolder(el) {
  const r = await api('/api/open', {id: el.dataset.id, rel: el.dataset.rel, target: el.dataset.target || ''});
  if (r.ok) toast('已打开：' + r.path);
}


/* ---- 移动会话：umi_normal ⇄ umi_calibration（同层级路径，只改名移动）。先预览、勾选，再确认 ----
   ISuit-V3：自动判定为标定的错放数据（openMove 无参数）；UMI_v3：用户看过视频后手动选择方向（data-to=calibration|normal，对勾选的会话） */
async function openMove(el) {
  if (S.busy || S.scanning) return toast('扫描/任务进行中，暂时不能移动');
  const to = (el && el.dataset && el.dataset.to) || '';
  if (to) return openMoveManual(S.D.filter(r => S.sel.has(r.id) && isUmi(r) && r.type !== to).map(r => r.id), to);
  const j = await api('/api/move/plan'); if (!j.plans) return;
  S.MV = j.plans; S.moveTo = ''; renderMove(); openModal('mm');
}
async function openMoveManual(ids, to) {
  if (S.busy || S.scanning) return toast('扫描/任务进行中，暂时不能移动');
  if (!ids.length) return toast(to === 'calibration' ? '请先勾选要移到标定的 UMI_v3 常规会话（点会话名可预览视频）' : '请先勾选要移回常规的 UMI_v3 标定会话');
  const j = await api('/api/move/preview', {ids, to}); if (!j.plans) return;
  S.MV = j.plans; S.moveTo = to; renderMove(); openModal('mm');
}
const DIRS = {normal: 'umi_normal', calibration: 'umi_calibration'};
function renderMove(res) {
  const byId = new Map(S.D.map(r => [r.id, r])), P = S.MV || [], man = !!S.moveTo, to = S.moveTo || 'calibration';
  $('mmTitle').textContent = !man ? '移动错放的标定数据' : to === 'calibration' ? '移到标定（手动选择）' : '移回常规（手动选择）';
  $('mmDesc').textContent = !man ? 'umi_normal 下被判定为标定（时长 < 3 分钟 / 任务名含「标定」/ 采集配置声明为标定）的会话，移动到 umi_calibration 的同层级目录。只做改名移动，不复制、不覆盖。'
    : `把勾选的会话从 ${DIRS[to === 'calibration' ? 'normal' : 'calibration']} 移到 ${DIRS[to]} 的同层级目录。只做改名移动，不复制、不覆盖。`;
  $('mbody').innerHTML = P.length ? `<table><thead><tr><th><input type="checkbox" id="mvall" checked></th><th>会话目录</th><th>任务名</th><th>${man ? '时长' : '判定依据'}</th><th>目标路径</th><th>提示</th></tr></thead><tbody>`
    + P.map(p => { const r = byId.get(p.id) || {}, up = r.up_state === '已上传';
      return `<tr><td><input type="checkbox" data-mv="${esc(p.id)}" ${p.blocked ? 'disabled' : 'checked'}></td><td title="${esc(p.source)}">${esc(p.name)}</td><td>${esc(r.task)}</td><td>${man ? hms(r.duration) : esc(p.reason)}</td><td class="muted" title="${esc(p.target)}">${esc(p.target)}</td>`
        + `<td>${p.blocked ? tag('不能移动', 'bad', p.blocked) : up ? tag('已上传', 'wn', `已按 ${DIRS[p.to === 'calibration' ? 'normal' : 'calibration']} 路径上传过；移动后 OSS 路径变为 ${DIRS[p.to]}，需要重新上传，OSS 上旧路径的数据需自行处理`) : ''}</td></tr>`; }).join('') + '</tbody></table>'
    : `<div class="empty">${man ? '没有可移动的会话' : '没有需要移动的数据（umi_normal 下没有被判定为标定的会话）'}</div>`;
  $('mres').innerHTML = res ? `<div class="ok-text">已移动 ${res.moved.length} 个</div>` + res.failed.map(f => `<div class="e-text">✘ ${esc(f.name)}：${esc(f.error)}</div>`).join('') : '';
  $('mgo').disabled = !P.some(p => !p.blocked);
}
async function doMove() {
  const ids = [...$('mbody').querySelectorAll('input[data-mv]:checked')].map(e => e.dataset.mv), to = S.moveTo || 'calibration', from = to === 'calibration' ? 'normal' : 'calibration';
  if (!ids.length) return toast('请勾选要移动的数据');
  if (!confirm(`把 ${ids.length} 个会话从 ${DIRS[from]} 移动到 ${DIRS[to]}？\n\n只移动文件夹（同盘改名，不复制、不覆盖），原位置不会留下数据；留下的空目录不会删除。`)) return;
  $('mgo').disabled = true;
  const r = await api('/api/move', S.moveTo ? {ids, to: S.moveTo} : {ids});
  if (!r.moved) { $('mgo').disabled = false; return; }
  r.moved.forEach(m => S.sel.delete(m.id));
  S.MV = S.MV.filter(p => !r.moved.some(m => m.id === p.id));
  toast(`已移动 ${r.moved.length} 个` + (r.failed.length ? `，${r.failed.length} 个失败` : '') + (r.moved.some(m => m.rescan) ? '（部分需重新扫描）' : ''));
  await poll(); renderMove(r);
}
