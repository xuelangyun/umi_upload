/* 页面上方的各个面板：应用栏控件、中断提示、数据源卡片、进度卡片、日志。只负责「把状态画出来」。 */
const CFG_KEY = {order: 'order', ds_jobs: 'ds_jobs'};   // 配置项 → /api/state 里的字段

function renderControls(s) {
  document.querySelectorAll('[data-act=cfg]').forEach(b => b.classList.toggle('on', String(s[CFG_KEY[b.dataset.k]]) === b.dataset.v));
  document.querySelectorAll('[data-show]').forEach(e => e.hidden = e.dataset.show !== s.method);      // 方式相关的控件
}

/* 数据源卡片上的「格式 → 桶」：目录名决定会话格式，格式决定上传到哪个桶（两种格式可同时使用） */
function renderBuckets(s) {
  S.buckets = s.buckets || {}; S.formats = s.formats || {};
  const el = $('bkInfo');
  const html = Object.keys(S.buckets).map(f => `<span class="tag f-${f === 'umi_v3' ? 'umi' : 'isuit'}" title="${esc(S.formats[f])} 格式的数据上传到这个桶；在「配置」里修改">${esc(S.formats[f] || f)} → ${esc(S.buckets[f])}</span>`).join('');
  if (el.innerHTML !== html) el.innerHTML = html;
}

function renderNotice(n) {
  const e = $('notice');
  if (!n) { if (!e.hidden) e.hidden = true; return; }
  if (e.hidden) e.hidden = false;
  const html = `<span>⚠ 上次${{upload: '上传', verify: '快速校验', verify_deep: '深度校验'}[n.action] || '任务'}被中断（${esc(n.started_at)}，已完成 ${n.ok}/${n.total}）。已传完的数据不受影响，重新「上传选中」会跳过已完成的、未完成的从断点继续。</span><span class="grow"></span><button class="btn small" data-act="ackNotice">知道了</button>`;
  if (e.innerHTML !== html) e.innerHTML = html;
}

function renderRoots(roots) {
  const el = $('roots');
  const html = roots.map(r => `<span class="chip" title="${esc(r)}">${esc(r)}<i data-act="rmRoot" data-r="${esc(r)}">✕</i></span>`).join('');
  if (el.innerHTML !== html) el.innerHTML = html;
}

/* 城市+房间 → 上传路径里的 PRT 目录预览 */
function prt() {
  const c = ($('city').value || '').toUpperCase().replace(/[^A-Z]/g, ''), r = ($('room').value || '').replace(/\D/g, '');
  const e = $('prtp'); e.classList.toggle('ov', !!(c && r));
  e.textContent = c && r ? '覆盖本地 → PRT_' + c + '_room' + r : '按本地目录名';
}

/* 扫描中的按钮/提示状态（防重复点击） */
function setScanUi(sc, nRoots) {
  const running = !!sc.running;
  S.scanning = running;
  const button = $('scanBtn');
  if (button.dataset.scanning !== String(running)) {
    button.dataset.scanning = String(running);
    button.innerHTML = running ? '<span class="spin"></span> 扫描中…' : '扫描';
  }
  const scanDisabled = running || S.busy;
  if (button.disabled !== scanDisabled) button.disabled = scanDisabled;
  const addButton = $('addBtn');
  if (addButton.disabled !== running) addButton.disabled = running;
  const wrapper = $('wr');
  if (wrapper.classList.contains('sc') !== running) wrapper.classList.toggle('sc', running);
  setPanelText('sinfo', running ? `扫描中 · ${sc.stage}${sc.total ? ` ${sc.done}/${sc.total}` : ''} · ${sc.elapsed}s` : `${nRoots} 个目录 · ${S.D.length} 个会话`);
}

function renderProblemsButton() {
  const nw = datasetOverview().stats.warn, n = S.PR.length + nw, b = $('probBtn');
  b.hidden = !n; b.textContent = '⚠ 提示 ' + n;
  return nw;
}

/* 进度卡片 + 日志。j = /api/state.job */
function showJob(j) {
  if (!j) return;
  const p = j.prog, name = {upload: '上传', verify: '快速校验', verify_deep: '深度校验'}[j.action];
  setPanelText('ptitle', (j.done ? '已完成 · ' : '') + name + ` · 完成 ${p.done_n}/${p.ds_n}` + (j.done ? '' : ` · 进行中 ${p.running}`) + ` · 成功 ${p.ok} 失败 ${p.fail}`);
  setPanelText('pct', p.pct.toFixed(1) + '%');
  const bar = $('bar1'), width = p.pct + '%';
  if (bar.style.width !== width) bar.style.width = width;
  const running = !j.done, progressBar = bar.parentNode;
  if (progressBar.classList.contains('run') !== running) progressBar.classList.toggle('run', running);
  setPanelText('spd', j.done ? '—' : (p.rep_speed ? sz(p.rep_speed) : sz(p.speed)) + '/s');
  setPanelText('sent', j.done ? '—' : sz(p.sent) + ' / ' + sz(p.total));
  setPanelText('fil', j.done ? '—' : `${p.fdone}/${p.ftotal} · ${p.files}/${p.total_files}`);
  setPanelText('eta', hms(p.elapsed) + ' / ' + (p.eta != null ? hms(p.eta) : '—'));
  setPanelText('pl', j.done ? '已结束 · 平均速度 ' + sz(p.avg) + '/s'
    : `${p.phase}${p.running > 1 ? ` · ${p.running} 个数据集并发` : ''} · 哈希 ${p.total ? (100 * p.hashed / p.total).toFixed(0) : 0}% · ${p.file || p.name}` + (p.raw ? ` ｜ ${p.raw}` : ''));
  renderLog(j.log);
}

let pendingLogLines = null;

// 日志被选中时暂缓 DOM 更新，避免 SSE 增量追加/清理节点打断文本选择和复制。
document.addEventListener('selectionchange', () => {
  if (pendingLogLines === null) return;
  const selection = window.getSelection();
  const log = $('lg');
  const selectingLog = selection && !selection.isCollapsed &&
    [...selection.getRangeAt(0).commonAncestorContainer.parentNode ? [selection.getRangeAt(0).commonAncestorContainer] : []]
      .some(node => node === log || log.contains(node));
  if (!selectingLog) {
    const lines = pendingLogLines;
    pendingLogLines = null;
    renderLog(lines);
  }
});

function renderLog(lines) {
  const log = $('lg'), onlyErrors = $('onlye').checked;
  const previous = S.lastLogSource || [], next = Array.isArray(lines) ? lines : [];
  if (next === S.lastLogSource && onlyErrors === S.lastLogOnly) return;

  const selection = window.getSelection();
  if (selection && !selection.isCollapsed && selection.rangeCount) {
    const range = selection.getRangeAt(0);
    const anchor = selection.anchorNode, focus = selection.focusNode;
    const isInLog = node => !!node && (node === log || log.contains(node.nodeType === Node.ELEMENT_NODE ? node : node.parentNode));
    if (isInLog(anchor) || isInLog(focus)) {
      pendingLogLines = next;
      return;
    }
  }
  pendingLogLines = null;

  const isVisible = line => !onlyErrors || /✘|错误|失败/.test(line);
  const createFragment = values => {
    const fragment = document.createDocumentFragment();
    for (const line of values) {
      if (!isVisible(line)) continue;
      const item = document.createElement('div');
      item.className = /✘|\[错误\]|失败/.test(line) ? 'e' : /✔/.test(line) ? 'o' : /▶ \[/.test(line) ? 'h' : '';
      item.textContent = line;
      fragment.append(item);
    }
    return fragment;
  };

  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 10;
  const sameMode = onlyErrors === S.lastLogOnly;
  let updatedIncrementally = false;

  if (sameMode && previous.length) {
    let overlap = Math.min(previous.length, next.length);
    while (overlap > 0) {
      const offset = previous.length - overlap;
      let matches = true;
      for (let i = 0; i < overlap; i++) {
        if (previous[offset + i] !== next[i]) {
          matches = false;
          break;
        }
      }
      if (matches) break;
      overlap--;
    }

    if (overlap > 0) {
      const removeCount = previous.length - overlap;
      const visibleRemoved = onlyErrors ? previous.slice(0, removeCount).filter(isVisible).length : removeCount;
      for (let i = 0; i < visibleRemoved && log.firstElementChild; i++) log.firstElementChild.remove();
      log.append(createFragment(next.slice(overlap)));
      updatedIncrementally = true;
    }
  }

  if (!updatedIncrementally) log.replaceChildren(createFragment(next));
  S.lastLogSource = next;
  S.lastLogOnly = onlyErrors;
  if (atBottom) log.scrollTop = log.scrollHeight;
}

/* 独立带宽状态：指标实时刷新，最近一次并发调整常驻显示 */
const BW_STATE_LABEL = {
  idle: '等待任务', monitoring: '监控中', waiting: '等待上传',
  manual: '普通模式', holding: '保持并发',
  'increasing-files': '文件并发已增加',
  'increasing-parts': '分片并发已增加',
  'task-limited': '文件不足', 'limit-reached': '达到上限'
};
function setPanelText(id, value) {
  const el = $(id), text = String(value);
  if (el && el.textContent !== text) el.textContent = text;
}

function setBandwidthText(id, value) {
  setPanelText(id, value);
}
function renderBandwidth(b) {
  if (!b) return;
  const state = b.state || 'idle';
  const active = state !== 'idle';
  const enabled = b.enabled === true;
  setBandwidthText('bwState', active && !enabled ? '普通模式' : (BW_STATE_LABEL[state] || state));
  const stateEl = $('bwState');
  const stateClass = 'bw-state' + (active && enabled && !['waiting','manual','limit-reached','task-limited'].includes(state) ? ' running' : ['waiting','task-limited','limit-reached'].includes(state) ? ' waiting' : '');
  if (stateEl.className !== stateClass) stateEl.className = stateClass;
  setBandwidthText('bwSpeed', Number(b.current_mbps || 0).toFixed(1) + ' Mbps');
  setBandwidthText('bwTarget', !active ? '—' : !enabled ? '关闭' : (Number(b.target_mbps || 0) > 0 ? Number(b.target_mbps).toFixed(0) + ' Mbps' : '自动'));
  setBandwidthText('bwFiles', Number(b.active_files || 0) + ' / ' + Number(b.file_limit || 0));
  setBandwidthText('bwParts', String(Number(b.parallel || 0)));
  setBandwidthText('bwChange', b.change || (active ? '尚未发生并发调整' : '等待上传任务'));
  const card = $('bandwidthCard'), tuning = active && enabled;
  if (card.classList.contains('tuning') !== tuning) card.classList.toggle('tuning', tuning);
}
