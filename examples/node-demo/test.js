const test = require("node:test");
const assert = require("node:assert/strict");
const { parseItems } = require("./server");

test("empty string returns empty list", () => {
  assert.deepEqual(parseItems("   "), []);
});

test("comma separated items are trimmed", () => {
  assert.deepEqual(parseItems("a, b, c"), ["a", "b", "c"]);
});

test("trailing comma does not produce blank item", () => {
  assert.deepEqual(parseItems("a, b,"), ["a", "b"]);
});
