/* UMI_v3 视频预览：点数据列表里的会话名弹出。UMI_v3 的标定会话无法自动区分，要看视频里有没有二维码（标定视频通常 < 3 分钟）。
   视频来自本机：/api/videos 列出会话里的视频，/api/video 以 Range 方式流式返回（可拖动进度）。看完可直接「移到标定 / 移回常规」。
   采集视频是 H.265：Edge / 新版 Chrome（需硬件解码，Windows 可能要装「HEVC 视频扩展」）/ Safari 能播；播不了时点「打开文件夹」用本机播放器。 */
const V = {id: '', files: [], rate: 1};
const hevcOk = () => !!document.createElement('video').canPlayType('video/mp4; codecs="hvc1.1.6.L93.B0"');

function stopVideo() {                                   // 释放视频文件：Windows 下文件被占用会导致移动文件夹失败
  const p = $('vplayer'); p.pause(); p.removeAttribute('src'); p.load();
}
async function openVideo(el) { openModal('vm'); await showVideo(el.dataset.id); }
async function showVideo(id) {
  const row = S.D.find(r => r.id === id); if (!row) return;
  V.id = id; stopVideo(); $('verr').textContent = ''; $('vdur').textContent = ''; $('vsel').innerHTML = '';
  $('vtitle').textContent = row.name;
  $('vinfo').innerHTML = `${tag(row.type === 'normal' ? '常规' : '标定', row.type === 'normal' ? '' : 'wn')} 采集时长 <b>${hms(row.duration)}</b>${shortCal(row) ? ' ' + tag('<3分钟', 'wn', '时长不足 3 分钟，很可能是标定') : ''} · ${esc(row.room)} · ${esc(row.start)} · <span class="muted" title="${esc(row.source)}">${esc(row.source)}</span>`;
  $('vmove').textContent = row.type === 'normal' ? '移到标定' : '移回常规'; $('vmove').dataset.to = row.type === 'normal' ? 'calibration' : 'normal';
  const j = await api('/api/videos?id=' + encodeURIComponent(id)); if (V.id !== id) return;
  if (!j.files) { $('verr').textContent = j.error || '读取视频列表失败'; return; }
  V.files = j.files;
  if (!V.files.length) { $('verr').textContent = '这个会话里没有找到视频文件（head / left hand / right hand 下的 .mp4）'; return; }
  $('vsel').innerHTML = V.files.map((f, i) => `<option value="${i}">${esc(f.f)}（${sz(f.size)}）</option>`).join('');
  if (!hevcOk()) $('verr').textContent = '提示：当前浏览器可能不支持 H.265 视频。请用 Edge / 新版 Chrome（Windows 需「HEVC 视频扩展」）或 Safari；播不了可点「打开文件夹」用本机播放器。';
  playVideo(0);
}
function playVideo(i) {
  const f = V.files[i], p = $('vplayer'); if (!f) return;
  $('vsel').value = i; p.src = `/api/video?id=${encodeURIComponent(V.id)}&f=${encodeURIComponent(f.f)}`; p.load();
}
$('vplayer').addEventListener('loadedmetadata', e => {
  const p = e.target; p.playbackRate = V.rate; $('verr').textContent = '';
  $('vdur').textContent = `这个视频的播放时间：${hms(p.duration)}` + (p.duration < 180 ? '（不到 3 分钟，通常是标定）' : '');
});
$('vplayer').addEventListener('error', () => { if ($('vplayer').getAttribute('src')) $('verr').textContent = '这个视频浏览器播不了（多半是 H.265 编码不被支持）。请点「打开文件夹」用本机播放器看。'; });
$('vsel').addEventListener('change', e => playVideo(+e.target.value));
$('vrate').addEventListener('change', e => { V.rate = +e.target.value; $('vplayer').playbackRate = V.rate; });
function stepVideo(el) {                                 // 在当前（筛选后的）列表里切到上一个 / 下一个会话
  const i = S.R.findIndex(r => r.id === V.id), n = S.R[i + +el.dataset.d];
  if (n) showVideo(n.id); else toast(+el.dataset.d > 0 ? '已经是最后一个' : '已经是第一个');
}
function closeVideo() { stopVideo(); V.id = ''; closeModal('vm'); }
async function videoMove(el) {                           // 先停播放释放文件，再走统一的「移动预览 → 确认」
  const id = V.id, to = el.dataset.to; closeVideo(); await openMoveManual([id], to);
}
async function videoFolder() {
  const r = S.D.find(x => x.id === V.id); if (r) await openFolder({dataset: {id: r.id, rel: r.rel, target: 'session'}});
}
