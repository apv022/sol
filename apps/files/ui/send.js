// Pure rules of "Send to a person" for the Files UI: which entries can be sent, how a send is
// described and how people are ordered in the picker. No DOM and no calls, so they are tested
// in a browser page (test/send.test.js) without a daemon.

/** The most files one send may carry (the app refuses more). */
export const MAX_SEND = 100;

/** Whether these list entries can be sent: files only (folders are shared instead), my own. */
export function sendable(es) {
  return es.length > 0 && es.length <= MAX_SEND
    && es.every((e) => e.type === 'file' && !e.virtual && !e.copy && !String(e.path).startsWith('Shared/'));
}

/** "a.jpg" for one file, "3 files" for several. */
export function sendWhat(items) {
  return items.length === 1 ? items[0].name : items.length + ' files';
}

/** The words for what became of a send. */
export function statusLabel(status) {
  return { sent: 'Waiting for them', saved: 'Saved', declined: 'Declined', pending: 'Waiting for you' }[status] || status;
}

/** People for the picker: the ones online first, then by name. */
export function orderPeople(ppl) {
  const nm = (p) => (p.petname || p.name || '').toLowerCase();
  return [...ppl].sort((a, b) => (b.online ? 1 : 0) - (a.online ? 1 : 0) || nm(a).localeCompare(nm(b)));
}

/** The name to show for a contact. */
export const personLabel = (p) => p.petname || p.name || p.id;

/** Incoming sends still waiting for an answer, newest first (the app already sorts them). */
export const pendingIn = (received) => (received || []).filter((r) => r.status === 'pending');

/** The newest few outgoing sends, for the small "Sent" list. */
export const recentOut = (sent, n = 5) => (sent || []).slice(0, n);
