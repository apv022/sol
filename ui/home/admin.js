// Client for the admin API (design/ADMIN.md). Same-origin fetch with the session cookie; no
// imports so it can be tested with a stub fetch. A 401 calls onUnauthorized, so the page can show
// the "Open Sol from the app or run `sol open`" screen. Errors use the API.md shape.

/** An admin API failure: `code` and `message` from the daemon, `status` the HTTP status (0 offline). */
export class AdminError extends Error {
  constructor(code, message, status) {
    super(message || code);
    this.name = 'AdminError';
    this.code = code;
    this.status = status;
  }
}

const enc = encodeURIComponent;

/**
 * Builds the client. Options: fetch (defaults to the global one, looked up per call), base
 * (default /admin/v1), onUnauthorized (called once per 401), EventSource (for tests).
 */
export function createAdmin({ fetch: f, base = '/admin/v1', onUnauthorized, EventSource: ES } = {}) {
  async function req(method, path, json) {
    const init = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
    if (json !== undefined) { init.body = JSON.stringify(json); init.headers['Content-Type'] = 'application/json'; }
    let res;
    try { res = await (f || globalThis.fetch.bind(globalThis))(base + '/' + path, init); } catch (e) {
      throw new AdminError('unavailable', 'Cannot reach Sol: ' + (e && e.message), 0);
    }
    const text = await res.text();
    let data = null;
    if (text) { try { data = JSON.parse(text); } catch { data = null; } }
    if (!res.ok) {
      if (res.status === 401 && onUnauthorized) onUnauthorized();
      const err = (data && data.error) || {};
      throw new AdminError(err.code || (res.status === 401 ? 'unauthorized' : 'unavailable'), err.message || res.statusText, res.status);
    }
    return data;
  }
  const get = (p) => req('GET', p);
  const post = (p, b = {}) => req('POST', p, b);

  return {
    status: () => get('status'),
    people: () => get('people'),
    renamePerson: (id, petname) => req('PATCH', `people/${enc(id)}`, { petname }),
    unpair: (id) => req('DELETE', `people/${enc(id)}`),
    devices: () => get('devices'),
    updateDevice: (id, patch) => req('PATCH', `devices/${enc(id)}`, patch),
    removeDevice: (id) => req('DELETE', `devices/${enc(id)}`),
    invite: (join = false) => post('invite', { join }),
    pair: (link) => post('pair', { link }),
    apps: () => get('apps'),
    installApp: (dir) => post('apps/install', { dir }),
    removeApp: (id) => post(`apps/${enc(id)}/remove`),
    restartApp: (id) => post(`apps/${enc(id)}/restart`),
    services: () => get('services'),
    addService: (s) => post('services', s),
    updateService: (name, patch) => req('PATCH', `services/${enc(name)}`, patch),
    removeService: (name) => req('DELETE', `services/${enc(name)}`),
    tunnels: () => get('tunnels'),
    openTunnel: (t) => post('tunnels', t),
    closeTunnel: (id) => req('DELETE', `tunnels/${enc(id)}`),
    inbox: () => get('inbox'),
    approve: (id) => post(`inbox/${enc(id)}/approve`),
    deny: (id) => post(`inbox/${enc(id)}/deny`),
    theme: () => get('theme'),
    themes: () => get('themes'),
    setTheme: (name) => post('theme', { name }),
    themeSettings: (s) => post('theme/settings', s),
    backup: () => get('backup'),
    confirmBackup: () => post('backup/confirm'),
    agents: () => get('agents'),
    revokeAgent: (name) => post(`agents/${enc(name)}/revoke`),
    // Not in the endpoint table yet: revoking one browser session (Settings lists them).
    revokeSession: (id) => req('DELETE', `sessions/${enc(id)}`),
    logout: () => post('logout'),
    networkChanged: () => post('network-changed'),

    /**
     * Opens the live event stream. `onEvent(type, data)` gets each event (status, person, device,
     * session, app, inbox, service, tunnel, theme); EventSource resumes with Last-Event-ID by
     * itself. Returns the EventSource so the caller can close it.
     */
    events(onEvent, { onError, onOpen } = {}) {
      const types = ['status', 'person', 'device', 'session', 'app', 'inbox', 'service', 'tunnel', 'theme'];
      const src = new (ES || globalThis.EventSource)(base + '/events', { withCredentials: true });
      for (const t of types) {
        src.addEventListener(t, (e) => {
          let d = null;
          try { d = e.data ? JSON.parse(e.data) : null; } catch { d = null; }
          onEvent(t, d);
        });
      }
      if (onError) src.onerror = onError;
      if (onOpen) src.onopen = onOpen;
      return src;
    },
  };
}
