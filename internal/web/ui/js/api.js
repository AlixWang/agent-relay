// API client helper for agent-relay console
export async function api(path, opts = {}) {
  const o = { headers: { 'Content-Type': 'application/json' }, ...opts };
  o.headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  const r = await fetch(path, o);
  let body = null;
  try { body = await r.json(); } catch { body = null; }
  if (!r.ok) {
    const msg = (body && body.error) || r.statusText || ('HTTP ' + r.status);
    const err = new Error(msg);
    err.status = r.status;
    err.body = body;
    throw err;
  }
  return body;
}
