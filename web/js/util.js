/* 通用工具与全局状态。无依赖，最先加载。
   S = 前端全局状态（所有文件共享）；改状态字段请在这里登记，方便后来人查找。 */
const $ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const hms = s => { s = Math.round(s || 0); return [s / 3600 | 0, s / 60 % 60 | 0, s % 60].map(x => String(x).padStart(2, '0')).join(':'); };
const sz = n => n >= 2 ** 30 ? (n / 2 ** 30).toFixed(2) + ' GB' : n >= 2 ** 20 ? (n / 2 ** 20).toFixed(1) + ' MB' : ((n || 0) / 1024).toFixed(0) + ' KB';

const S = {
  D: [],            // 数据集（/api/state.datasets）
  RH: [], RS: {total: 0}, facets: {},   // 上传记录（/api/uploads 的明细/汇总/下拉选项）
  F: {},            // 已生效的过滤条件（点「查询」才更新）
  view: 1,          // 1 数据列表 / 2 上传记录
  sel: new Set(),   // 勾选的数据集 id
  sortK: 'start', sortD: -1, page: 0,
  R: [],            // 数据列表当前过滤+排序后的全部行（含所有页）
  busy: false,      // 有上传/校验任务在运行
  scanning: false, prevScan: false,
  PR: [],           // 扫描问题
  lastLog: '', sig: {}, bcur: '', stopped: false, timer: null,
  buckets: {}, formats: {},   // 两种数据格式各自的桶 {isuit: 'hw-upload', umi_v3: 'prt-huawei'} / 格式名（/api/state）；会话格式由目录名决定，每行 r.fmt
  loaded: false, lastFull: 0,   // 最近一次完整状态快照
  roots: [], jobID: '', jobLog: [], events: null, liveRefresh: false, sseFallbackTimer: null,
  pollPromise: null, pollAgain: false, datasetETag: '',
};

/* 状态文字 → 样式类（tag 颜色） */
const OK_SET = ['一致', '已上传', '成功', '已存在', 'done'], BAD_SET = ['不一致', '上传失败', '失败', '出错', '远端缺失', 'fail'];
const cls = s => OK_SET.includes(s) ? 'ok' : BAD_SET.includes(s) ? 'bad' : s === '未上传' ? '' : 'wn';
const tag = (text, c, title) => `<span class="tag ${c ?? cls(text)}"${title ? ` title="${esc(title)}"` : ''}>${esc(text)}</span>`;

/* session 目录名里的日期（采集日期）与所在日期文件夹（归档日期）是否不一致 */
const roomBad = room => !!room && !/^PRT_[A-Za-z]+_room\d{3}$/.test(room);     // 房间目录不是 PRT_WX_room002（3 位）格式
const hasWarn = r => !!(r.date_warn || r.room_diff || r.room_bad || r.dev_diff);       // 数据行有任何提示
const isUmi = r => r.fmt === 'umi_v3';                                      // UMI_v3 会话：标定要看视频（二维码）后手动移动；ISuit-V3 自动判定
const fmtTag = f => tag(S.formats[f] || f, 'f-' + (f === 'umi_v3' ? 'umi' : 'isuit'), `会话格式 ${S.formats[f] || f} → 桶 ${S.buckets[f] || ''}`);
const shortCal = r => isUmi(r) && r.type === 'normal' && r.duration > 0 && r.duration < 180;   // UMI_v3：时长 < 3 分钟，很可能是标定（仅提示，不自动判定）
const movable = r => r.type === 'normal' && !!r.misplaced;                // umi_normal 下被判定为标定的数据（可「移到标定」）
const rowWarn = r => !!(r.date_warn || r.room_diff);                      // 整行标黄：日期不一致 / 房间被覆盖
/* 采集日期：取会话目录名里的日期。ISuit-V3_YYYY-MM-DD-HH-MM-SS（桶 hw-upload）与 UMI_v3_YYYYMMDD_HHMMSS（桶 prt-huawei）两种命名都认 */
const capOf = rel => {
  const n = String(rel).split('/').pop(), a = n.match(/_(\d{4}-\d{2}-\d{2})-\d{2}-\d{2}-\d{2}$/), u = n.match(/^UMI_v3_(\d{4})(\d{2})(\d{2})_\d{6}$/);
  return a ? a[1] : u ? `${u[1]}-${u[2]}-${u[3]}` : '';
};
const dwOf = (cap, dir) => !!(cap && dir && cap.replace(/-/g, '') !== dir);

function toast(msg) {
  const e = $('toast'); e.textContent = msg; e.style.display = 'block';
  clearTimeout(e._t); e._t = setTimeout(() => e.style.display = 'none', Math.min(3000 + msg.length * 60, 9000));
}
