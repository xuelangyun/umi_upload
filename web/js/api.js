/* 与后端通信。所有接口见 README「HTTP API」。 */
async function api(url, body) {
  const r = await fetch(url, body ? {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)} : {});
  const j = await r.json();
  if (!r.ok && j.error) toast(j.error);          // 统一在这里提示错误；调用方用 j.ok 判断成功
  return j;
}
async function setCfg(patch) { await api('/api/config', patch); poll(); }
