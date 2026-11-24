// Pure rules of Trash and versions for the Files UI: how a deleted item and an earlier version
// are described, and when the page should look again. No DOM and no calls, so they are tested in
// a browser page (test/trash.test.js) without a daemon.

const DAY = 864e5;

/** Where a deleted item was: its folder, or "My files" at the top. */
export function trashWhere(row) {
  return row.folder || 'My files';
}

/** What a Trash row holds: "3 files" for a folder, nothing for a single file. */
export function trashWhat(row) {
  if (!row.dir) return '';
  const n = row.files || 0;
  return n === 0 ? 'Empty folder' : n === 1 ? '1 file' : n + ' files';
}

/** Who deleted it: the device, and the person when it was not me. */
export function trashWho(row) {
  const dev = row.device || 'a device';
  const how = row.disk ? 'removed on ' : 'deleted on ';
  return row.by ? row.by + ', ' + how + dev : how[0].toUpperCase() + how.slice(1) + dev;
}

/** "Gone in 12 days", "Gone tomorrow", "Gone today": when Trash forgets it. */
export function goneIn(expiresAt, now = Date.now()) {
  const days = Math.ceil((expiresAt - now) / DAY);
  if (days <= 0) return 'Gone today';
  if (days === 1) return 'Gone tomorrow';
  return 'Gone in ' + days + ' days';
}

/** The total of a Trash listing: "4 items · 12 MB" (size formatted by the caller's fmtSize). */
export function trashSummary(list, fmtSize) {
  const n = list.count || 0;
  if (!n) return 'Trash is empty';
  return (n === 1 ? '1 item' : n + ' items') + ' · ' + fmtSize(list.bytes || 0);
}

/**
 * The delay until the next expiry, for the one timer the page sets while Trash is open (it never
 * polls). null when nothing is waiting; capped so a far date does not overflow setTimeout.
 */
export function expiryDelay(list, now = Date.now()) {
  if (!list || !list.next_expiry) return null;
  return Math.min(Math.max(list.next_expiry - now, 0) + 500, 2 ** 31 - 1);
}

/** Whether a list entry has versions to look at: my own files, not conflict copies. */
export function hasVersions(e) {
  return !!e && e.type === 'file' && !e.virtual && !e.copy && !String(e.path).startsWith('Shared/');
}

/** "Replaced on laptop" / "Replaced by Bob on laptop". */
export function versionWho(v) {
  return 'Replaced ' + (v.by ? 'by ' + v.by + ' ' : '') + 'on ' + (v.device || 'a device');
}
