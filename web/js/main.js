/* 入口：事件委托（data-act / data-change）+ SSE 状态推送 + 页签/扫描/任务的主流程。
   新增一个按钮：HTML 里写 data-act="名字"，在下面 ACT 表里加对应函数。 */
const ACT = {
  cfg: el => setCfg({[el.dataset.k]: el.dataset.v}),
  openConfig, saveConfig: el => saveConfig(!!el.dataset.test), switchConfigProfile, saveConfigAsProfile, deleteConfigProfile, openBrowse, addRoots, openProblems, openGroups, shutdown, applyNet, setFmt, openFolder, openMove, doMove, openVideo, stepVideo, closeVideo, videoMove, videoFolder,
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
    if (!confirm(`上传 ${sel.length} 个数据集，约 ${sz(total)}？\n${dest}\n方式：${mode}，数据集并发参数以配置页为准` + warn)) return;
  }
  const r = await api('/api/run', {action, ids: [...S.sel], force: $('force').checked});
  if (r.ok) toast('任务已开始');
  poll();
}

/* 完整状态通过 HTTP 首次加载/操作后刷新；运行进度、扫描进度和带宽状态由 SSE 推送。 */
async function fetchDatasetSnapshot() {
  const headers = {};
  if (S.datasetETag) headers['If-None-Match'] = S.datasetETag;
  const response = await fetch('/api/datasets', {headers, cache: 'no-store'});
  if (response.status !== 304 && !response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || '读取数据集失败：HTTP ' + response.status);
  }
  const version = response.headers.get('X-Dataset-Version');
  if (version !== null) S.datasetVersion = Number(version) || S.datasetVersion || 0;
  S.datasetETag = response.headers.get('ETag') || S.datasetETag || '';
  if (response.status === 304) return S.D;
  const rows = await response.json();
  S.D = Array.isArray(rows) ? rows : [];
  return S.D;
}

function scheduleDatasetSnapshotRetry() {
  if (S.stopped || S.datasetRetryTimer) return;
  const delay = Math.min(S.datasetRetryDelay || 1000, 15000);
  S.datasetRetryDelay = Math.min(delay * 2, 15000);
  S.datasetRetryTimer = setTimeout(() => {
    S.datasetRetryTimer = null;
    refreshDatasetSnapshot(S.datasetRefreshTarget).catch(() => {});
  }, delay);
}

function refreshDatasetSnapshot(targetVersion) {
  const target = Number(targetVersion || 0);
  if (!Number.isFinite(target) || target <= (S.datasetVersion || 0)) return Promise.resolve();
  S.datasetRefreshTarget = Math.max(S.datasetRefreshTarget || 0, target);
  if (S.datasetRetryTimer) {
    clearTimeout(S.datasetRetryTimer);
    S.datasetRetryTimer = null;
  }
  if (S.datasetRefreshPromise) return S.datasetRefreshPromise;

  let fetchFailed = false;
  const request = (async () => {
    do {
      try {
        await fetchDatasetSnapshot();
        S.datasetRetryDelay = 1000;
      } catch (_) {
        // Retry independently of future SSE events; the old list stays visible.
        fetchFailed = true;
        return;
      }
      if (S.loaded) {
        const have = new Set(S.D.map(row => row.id));
        S.sel.forEach(id => have.has(id) || S.sel.delete(id));
        draw();
      }
    } while (!S.stopped && S.datasetVersion < S.datasetRefreshTarget);
  })();
  S.datasetRefreshPromise = request;
  const cleanup = () => {
    if (S.datasetRefreshPromise !== request) return;
    S.datasetRefreshPromise = null;
    if (S.stopped || S.datasetVersion >= S.datasetRefreshTarget) return;
    if (fetchFailed) scheduleDatasetSnapshotRetry();
    else refreshDatasetSnapshot(S.datasetRefreshTarget).catch(() => {});
  };
  request.then(cleanup, cleanup);
  return request;
}
async function poll() {
  if (S.stopped) return;
  if (S.pollPromise) {
    S.pollAgain = true;
    return S.pollPromise;
  }
  const request = (async () => {
    do {
      S.pollAgain = false;
      const was = S.busy;
      const [s, datasets] = await Promise.all([
        api('/api/state?light=1'),
        fetchDatasetSnapshot()
      ]);
      if (s && s.scan) {
        s.datasets = datasets;
        applyFullState(s, was);
      }
    } while (S.pollAgain && !S.stopped);
  })();
  S.pollPromise = request;
  try {
    await request;
  } finally {
    if (S.pollPromise === request) S.pollPromise = null;
  }
}

function applyFullState(s, was) {
  if (!s || !s.scan) return;
  S.loaded = true; S.lastFull = Date.now();
  S.D = s.datasets || []; S.PR = s.problems || []; S.roots = s.roots || [];
  S.busy = !!(s.job && !s.job.done); S.scanning = !!s.scan.running;
  const have = new Set(S.D.map(r => r.id)); S.sel.forEach(id => have.has(id) || S.sel.delete(id));
  renderControls(s); renderBuckets(s); renderNotice(s.notice); renderRoots(S.roots);
  const nw = renderProblemsButton(); setScanUi(s.scan, S.roots.length);
  $('sinfo').title = '数据库：' + s.db;
  if (S.prevScan && !s.scan.running) {
    const rs = s.scan.result || {};
    toast(rs.error ? '扫描失败：' + rs.error : `扫描完成：${Number(rs.count ?? 0)} 个会话（重新统计 ${Number(rs.scanned ?? 0)} · 缓存 ${Number(rs.cached ?? 0)}），${Number(rs.problems ?? 0)} 个问题` + (nw ? `，${nw} 个有日期/房间提示` : '') + `，用时 ${s.scan.elapsed}s`);
    if (!rs.error && (S.PR.length || nw)) openProblems();
  }
  S.prevScan = s.scan.running;
  if (!$('fs').options.length) setStatusOptions();
  if (S.view === 1) fillOptions();
  if (!S.roots.length) $('pl').textContent = '先点「＋ 添加目录」（可一次勾选多个盘符），再点「扫描」';
  if (s.job) {
    S.jobID = s.job.id; S.jobLog = (s.job.log || []).slice(-250);
    showJob({...s.job, log: S.jobLog});
  }
  if (s.bandwidth) renderBandwidth(s.bandwidth);
  (S.view === 2 && was && !S.busy) ? loadRecords() : draw();
}

function applyLiveState(s) {
  if (!s || !s.scan) return;
  const busyNow = !!(s.job && !s.job.done);
  // 任务或扫描刚结束时必须重新获取完整快照，刷新列表、记录和最终结果。
  if ((S.busy && !busyNow) || (S.prevScan && !s.scan.running)) {
    if (!S.liveRefresh) {
      S.liveRefresh = true;
      poll().catch(() => {}).finally(() => { S.liveRefresh = false; });
    }
    return;
  }
  if (s.dataset_version !== undefined) refreshDatasetSnapshot(s.dataset_version).catch(() => {});
  S.busy = busyNow; S.scanning = !!s.scan.running; S.prevScan = s.scan.running;
  setScanUi(s.scan, (S.roots || []).length);
  if (s.job) {
    const lines = Array.isArray(s.job.log) ? s.job.log : [];
    if (s.job.log_reset || S.jobID !== s.job.id) S.jobLog = lines.slice(-250);
    else if (lines.length) S.jobLog = (S.jobLog || []).concat(lines).slice(-250);
    S.jobID = s.job.id;
    showJob({...s.job, log: S.jobLog || []});
  }
  if (s.bandwidth) renderBandwidth(s.bandwidth);
}

async function pollFallback() {
  try {
    const state = await api('/api/state?realtime=1');
    applyLiveState(state);
  } catch (_) {
    // Keep the last visible state; the next fallback cycle retries.
  }
}

function connectEvents() {
  if (typeof EventSource === 'undefined') {
    const fallback = () => { if (!S.stopped) pollFallback().finally(() => { S.timer = setTimeout(fallback, 1200); }); };
    fallback();
    return;
  }
  const source = new EventSource('/api/events');
  S.events = source;
  let opened = false;
  source.addEventListener('state', event => {
    try { applyLiveState(JSON.parse(event.data)); } catch (_) { /* Ignore malformed event; reconnect snapshot repairs state. */ }
  });
  source.onopen = () => {
    const reconnected = opened;
    opened = true;
    if (S.sseFallbackTimer) {
      clearTimeout(S.sseFallbackTimer);
      S.sseFallbackTimer = null;
    }
    // Retry the initial HTTP snapshot if it failed, and reconcile after every reconnect.
    if (!S.loaded || reconnected) poll().catch(() => {});
  };
  source.onerror = () => {
    // EventSource normally reconnects itself. While it is disconnected, keep a
    // low-frequency HTTP fallback so the dashboard does not freeze during a
    // proxy timeout or a server restart.
    if (!S.sseFallbackTimer) {
      const fallback = () => {
        if (S.stopped || source.readyState === EventSource.OPEN) {
          S.sseFallbackTimer = null;
          return;
        }
        pollFallback().finally(() => { S.sseFallbackTimer = setTimeout(fallback, 2000); });
      };
      S.sseFallbackTimer = setTimeout(fallback, 1000);
    }
  };
}

prt();
poll().catch(() => {}).finally(connectEvents);
