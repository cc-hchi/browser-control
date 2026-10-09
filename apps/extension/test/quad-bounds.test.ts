import assert from "node:assert/strict";
import test from "node:test";
import { quadBounds } from "../src/debugger-manager.js";

test("quadBounds returns the axis-aligned box of a CDP quad", () => {
  assert.deepEqual(quadBounds([10, 20, 110, 20, 110, 70, 10, 70]), {
    x: 10,
    y: 20,
    width: 100,
    height: 50,
  });
});

test("quadBounds covers a transformed (rotated) quad", () => {
  assert.deepEqual(quadBounds([50, 0, 100, 50, 50, 100, 0, 50]), {
    x: 0,
    y: 0,
    width: 100,
    height: 100,
  });
});

test("quadBounds rejects missing, short, degenerate and non-finite quads", () => {
  assert.equal(quadBounds(undefined), undefined);
  assert.equal(quadBounds([1, 2, 3]), undefined);
  assert.equal(quadBounds([5, 5, 5, 5, 5, 5, 5, 5]), undefined);
  assert.equal(quadBounds([0, 0, NaN, 0, 10, 10, 0, 10]), undefined);
});
