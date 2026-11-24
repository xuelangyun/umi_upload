/* 与后端通信。所有接口见 README「HTTP API」。 */

async function requestJSON(url, options = {}) {
  const response = await fetch(url, options);
  const payload = await response.json().catch(() => null);

  if (!response.ok) {
    const error = new Error(payload?.error || `请求失败：HTTP ${response.status}`);
    error.status = response.status;
    throw error;
  }
  if (payload === null || typeof payload !== 'object') {
    throw new Error('服务器返回了无效的 JSON 响应');
  }
  return payload;
}

async function api(url, body, {signal, quiet = false} = {}) {
  const options = {signal};
  if (body !== undefined) {
    options.method = 'POST';
    options.headers = {'Content-Type': 'application/json'};
    options.body = JSON.stringify(body);
  }

  try {
    return await requestJSON(url, options);
  } catch (error) {
    if (error.name === 'AbortError') throw error;
    const message = error.message || '网络请求失败';
    if (!quiet) toast(message);
    return {ok: false, error: message, status: error.status};
  }
}

async function setCfg(patch) {
  const result = await api('/api/config', patch);
  if (result.ok) poll();
}
