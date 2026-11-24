// Tests for the pure rules of Trash and versions (apps/files/ui/trash.js).
import { test, assert } from '../../../ui/kit/testing.js';
import { trashWhere, trashWhat, trashWho, goneIn, trashSummary, expiryDelay, hasVersions, versionWho } from '../ui/trash.js';

const DAY = 864e5;

test('where a deleted item was', () => {
  assert.equal(trashWhere({ folder: 'Docs/2026' }), 'Docs/2026');
  assert.equal(trashWhere({ folder: '' }), 'My files');
});

test('what a row holds', () => {
  assert.equal(trashWhat({ dir: false, files: 1 }), '');
  assert.equal(trashWhat({ dir: true, files: 0 }), 'Empty folder');
  assert.equal(trashWhat({ dir: true, files: 1 }), '1 file');
  assert.equal(trashWhat({ dir: true, files: 12 }), '12 files');
});

test('who deleted it, and how', () => {
  assert.equal(trashWho({ device: 'laptop' }), 'Deleted on laptop');
  assert.equal(trashWho({ device: 'laptop', disk: true }), 'Removed on laptop');
  assert.equal(trashWho({ device: 'phone', by: 'Bob' }), 'Bob, deleted on phone');
  assert.equal(trashWho({}), 'Deleted on a device');
});

test('when Trash forgets it', () => {
  const now = 1_800_000_000_000;
  assert.equal(goneIn(now + 12 * DAY, now), 'Gone in 12 days');
  assert.equal(goneIn(now + 11.2 * DAY, now), 'Gone in 12 days');
  assert.equal(goneIn(now + DAY, now), 'Gone tomorrow');
  assert.equal(goneIn(now + 3600e3, now), 'Gone tomorrow');
  assert.equal(goneIn(now - 1, now), 'Gone today');
});

test('summary line', () => {
  const size = (n) => n + ' B';
  assert.equal(trashSummary({ count: 0 }, size), 'Trash is empty');
  assert.equal(trashSummary({ count: 1, bytes: 5 }, size), '1 item · 5 B');
  assert.equal(trashSummary({ count: 3, bytes: 10 }, size), '3 items · 10 B');
});

test('one timer to the next expiry, never a poll', () => {
  const now = 1_000;
  assert.equal(expiryDelay(null, now), null);
  assert.equal(expiryDelay({ items: [] }, now), null);
  assert.equal(expiryDelay({ next_expiry: 3_000 }, now), 2_500);
  assert.equal(expiryDelay({ next_expiry: 500 }, now), 500); // already due: soon
  assert.equal(expiryDelay({ next_expiry: now + 400 * DAY }, now), 2 ** 31 - 1);
});

test('which entries have versions', () => {
  assert.ok(hasVersions({ type: 'file', path: 'Docs/a.md' }));
  assert.ok(!hasVersions({ type: 'dir', path: 'Docs' }));
  assert.ok(!hasVersions({ type: 'file', path: 'Shared/bob/x/a.md' }));
  assert.ok(!hasVersions({ type: 'file', path: 'a (conflict).md', copy: true }));
  assert.ok(!hasVersions(null));
});

test('who replaced a version', () => {
  assert.equal(versionWho({ device: 'laptop' }), 'Replaced on laptop');
  assert.equal(versionWho({ device: 'phone', by: 'Bob' }), 'Replaced by Bob on phone');
});
