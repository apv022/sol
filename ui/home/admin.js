// Client for the admin API (design/ADMIN.md). Same-origin fetch with the session cookie; no
// imports so it can be tested with a stub fetch. A 401 calls onUnauthorized, so the page can show
// the "This browser isn't signed in to Sol" screen. Errors use the API.md shape.

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
  // `raw` ({body, type}) sends the bytes as they are (an image upload) instead of JSON.
  async function req(method, path, json, raw, url = base + '/' + path) {
    const init = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
    if (raw) { init.body = raw.body; init.headers['Content-Type'] = raw.type; }
    else if (json !== undefined) { init.body = JSON.stringify(json); init.headers['Content-Type'] = 'application/json'; }
    let res;
    try { res = await (f || globalThis.fetch.bind(globalThis))(url, init); } catch (e) {
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
    /** Anonymous: Tor only both ways, they only learn my onion; false makes them a buddy. */
    setAnonymous: (id, anonymous) => req('PATCH', `people/${enc(id)}`, { anonymous }),
    unpair: (id) => req('DELETE', `people/${enc(id)}`),
    devices: () => get('devices'),
    updateDevice: (id, patch) => req('PATCH', `devices/${enc(id)}`, patch),
    removeDevice: (id) => req('DELETE', `devices/${enc(id)}`),
    invite: (join = false, anonymous = false) => post('invite', { join, anonymous }),
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
    /** The installed apps' actions (`[{id, name, actions: [{name, description, effect}]}]`), for picking an agent's scopes. */
    actions: () => get('actions'),
    /**
     * Makes an agent key: `{name, scopes, days}`. The answer is `{agent, key_file, path}` and the
     * key file's text is only ever returned here, once; the caller must not store it.
     */
    newAgent: (a) => post('agents/new', a),
    revokeAgent: (name) => post(`agents/${enc(name)}/revoke`),
    // Not in the endpoint table yet: revoking one browser session (Settings lists them).
    revokeSession: (id) => req('DELETE', `sessions/${enc(id)}`),
    logout: () => post('logout'),
    /** "Remove Sol from this device": `confirm` is the person's name. Answers `{setup}`, the setup page's path with its token. */
    reset: (confirm) => post('reset', { confirm }),
    /** The locked screen's Sign in: asks the daemon to start a session for the computer's own user (root path, not /admin/v1). */
    signIn: () => req('POST', '', undefined, undefined, '/_sol/signin'),
    wallpapers: () => get('wallpapers'),
    /** Uploads an image (a File or Blob; png, jpeg or webp, at most 15 MB) as a wallpaper: `{id, url, setting}`. */
    addWallpaper: (file) => req('POST', 'wallpapers', undefined, { body: file, type: file.type }),
    removeWallpaper: (id) => req('DELETE', `wallpapers/${enc(id)}`),
    networkChanged: () => post('network-changed'),
    transports: () => get('transports'),
    setTransport: (name, enabled) => post(`transports/${enc(name)}`, { enabled }),

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
