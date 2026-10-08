import assert from "node:assert/strict";
import test from "node:test";
import { editingCommandsFor } from "../src/debugger-manager.js";

const META = 4;
const CONTROL = 2;
const SHIFT = 8;
const ALT = 1;

test("Cmd/Ctrl editing shortcuts map to native editor commands", () => {
  assert.deepEqual(editingCommandsFor(META, "a"), ["selectAll"]);
  assert.deepEqual(editingCommandsFor(CONTROL, "a"), ["selectAll"]);
  assert.deepEqual(editingCommandsFor(META, "A"), ["selectAll"]);
  assert.deepEqual(editingCommandsFor(META, "c"), ["copy"]);
  assert.deepEqual(editingCommandsFor(META, "x"), ["cut"]);
  assert.deepEqual(editingCommandsFor(META, "v"), ["paste"]);
  assert.deepEqual(editingCommandsFor(META, "z"), ["undo"]);
  assert.deepEqual(editingCommandsFor(META | SHIFT, "z"), ["redo"]);
});

test("non-shortcut key combinations produce no editor command", () => {
  assert.deepEqual(editingCommandsFor(0, "a"), []);
  assert.deepEqual(editingCommandsFor(SHIFT, "a"), []);
  assert.deepEqual(editingCommandsFor(META, "Enter"), []);
  assert.deepEqual(editingCommandsFor(META | ALT, "a"), []);
  assert.deepEqual(editingCommandsFor(META | CONTROL, "a"), []);
  assert.deepEqual(editingCommandsFor(META | SHIFT, "a"), []);
});
