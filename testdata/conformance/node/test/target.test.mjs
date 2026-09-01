import assert from "node:assert/strict";
import test from "node:test";

import { formatTarget } from "../src/target.mjs";

test("formats a validated conformance target", () => {
  assert.equal(formatTarget("ActionsCommunity/multirunner", "linux"), "ActionsCommunity/multirunner:linux");
});

test("rejects an invalid repository", () => {
  assert.throws(() => formatTarget("../multirunner", "linux"), /owner\/name/);
});

test("rejects an unsupported platform", () => {
  assert.throws(() => formatTarget("ActionsCommunity/multirunner", "macos"), /linux or windows/);
});
