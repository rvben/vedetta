'use strict';
// Run: node --test internal/api/static/domsync.test.js

const test = require('node:test');
const assert = require('node:assert');
const { syncChildren } = require('./domsync.js');

// A parent that records every node it detaches. Inserting a node that is
// already a child detaches it first, as the DOM does, and that is what costs a
// focused control its focus.
function fakeParent(initial) {
  const parent = {
    childNodes: initial.slice(),
    detached: [],
    removeChild(node) {
      const at = parent.childNodes.indexOf(node);
      assert.notStrictEqual(at, -1, 'removeChild of a node that is not a child');
      parent.childNodes.splice(at, 1);
      parent.detached.push(node);
    },
    insertBefore(node, ref) {
      const from = parent.childNodes.indexOf(node);
      if (from !== -1) {
        parent.childNodes.splice(from, 1);
        parent.detached.push(node);
      }
      const at = ref === null ? parent.childNodes.length : parent.childNodes.indexOf(ref);
      assert.notStrictEqual(at, -1, 'insertBefore a reference that is not a child');
      parent.childNodes.splice(at, 0, node);
    }
  };
  return parent;
}

test('an unchanged list detaches nothing', () => {
  const parent = fakeParent(['a', 'b', 'c']);
  syncChildren(parent, ['a', 'b', 'c']);
  assert.deepStrictEqual(parent.childNodes, ['a', 'b', 'c']);
  assert.deepStrictEqual(parent.detached, []);
});

test('a new first row leaves the rows already shown mounted', () => {
  const parent = fakeParent(['a', 'b']);
  syncChildren(parent, ['new', 'a', 'b']);
  assert.deepStrictEqual(parent.childNodes, ['new', 'a', 'b']);
  assert.deepStrictEqual(parent.detached, []);
});

test('a row inserted in the middle leaves its neighbours mounted', () => {
  const parent = fakeParent(['a', 'c']);
  syncChildren(parent, ['a', 'b', 'c']);
  assert.deepStrictEqual(parent.childNodes, ['a', 'b', 'c']);
  assert.deepStrictEqual(parent.detached, []);
});

test('only the rows that leave are detached', () => {
  const parent = fakeParent(['a', 'gone', 'b', 'old']);
  syncChildren(parent, ['a', 'b']);
  assert.deepStrictEqual(parent.childNodes, ['a', 'b']);
  assert.deepStrictEqual(parent.detached, ['gone', 'old']);
});

test('an empty parent is filled in order', () => {
  const parent = fakeParent([]);
  syncChildren(parent, ['a', 'b']);
  assert.deepStrictEqual(parent.childNodes, ['a', 'b']);
});

test('nothing wanted empties the parent', () => {
  const parent = fakeParent(['a', 'b']);
  syncChildren(parent, []);
  assert.deepStrictEqual(parent.childNodes, []);
});
