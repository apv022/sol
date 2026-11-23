// Tests for the pure rules of Send to a person (apps/files/ui/send.js).
import { test, assert } from '../../../ui/kit/testing.js';
import { sendable, sendWhat, statusLabel, orderPeople, pendingIn, recentOut, MAX_SEND } from '../ui/send.js';

const file = (name, extra = {}) => ({ type: 'file', name, path: 'Docs/' + name, ...extra });

test('files can be sent, folders and shared things cannot', () => {
  assert.ok(sendable([file('a.txt')]));
  assert.ok(sendable([file('a.txt'), file('b.txt')]));
  assert.ok(!sendable([]));
  assert.ok(!sendable([{ type: 'dir', name: 'Docs', path: 'Docs' }]));
  assert.ok(!sendable([file('a.txt'), { type: 'dir', name: 'd', path: 'd' }]));
  assert.ok(!sendable([file('a.txt', { path: 'Shared/bob/Photos/a.txt' })]));
  assert.ok(!sendable([file('a.txt', { virtual: true })]));
  assert.ok(!sendable([file('a (conflict).txt', { copy: true })]));
});

test('at most MAX_SEND files', () => {
  const many = (n) => Array.from({ length: n }, (_, i) => file('f' + i));
  assert.ok(sendable(many(MAX_SEND)));
  assert.ok(!sendable(many(MAX_SEND + 1)));
});

test('what was sent', () => {
  assert.equal(sendWhat([{ name: 'a.jpg' }]), 'a.jpg');
  assert.equal(sendWhat([{ name: 'a' }, { name: 'b' }, { name: 'c' }]), '3 files');
});

test('status words', () => {
  assert.equal(statusLabel('saved'), 'Saved');
  assert.equal(statusLabel('declined'), 'Declined');
  assert.equal(statusLabel('sent'), 'Waiting for them');
  assert.equal(statusLabel('odd'), 'odd');
});

test('people: online first, then by name', () => {
  const got = orderPeople([{ id: '1', name: 'Zoe' }, { id: '2', name: 'Bob', online: true }, { id: '3', petname: 'amy', name: 'Amelia' }, { id: '4', name: 'Cy', online: true }]);
  assert.equal(got.map((p) => p.id).join(''), '2431');
});

test('lists', () => {
  assert.equal(pendingIn([{ status: 'pending' }, { status: 'saved' }, { status: 'pending' }]).length, 2);
  assert.equal(pendingIn(undefined).length, 0);
  assert.equal(recentOut([1, 2, 3, 4, 5, 6, 7]).length, 5);
});
