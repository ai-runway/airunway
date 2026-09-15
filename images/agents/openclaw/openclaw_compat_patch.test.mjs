import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { patchOpenClaw } from "./openclaw_compat_patch.mjs";

test("backports visible length-limited Chat Completions into the pinned OpenClaw bundle", (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "airunway-openclaw-patch-"));
  const dist = path.join(root, "dist");
  fs.mkdirSync(dist);
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const selectionPath = path.join(dist, "selection-me3nXBxK.js");
  fs.writeFileSync(
    selectionPath,
    `prefix
function isIncompleteTerminalAssistantTurn(params) {
\tconst stopReason = params.lastAssistant?.stopReason;
\treturn stopReason === "toolUse" || stopReason === "length" && !params.hasTerminalOutput;
}
suffix`,
  );

  const openAiPath = path.join(dist, "openai-http-B9VYVAS4.js");
  fs.writeFileSync(
    openAiPath,
    `prefix
\t\t\t\t\tfinish_reason: "stop"
middle
\t\t\trequestFinalize();
\t\t} catch (err) {
\t\t\tresultResolved = true;
suffix`,
  );

  patchOpenClaw(root);

  const selection = fs.readFileSync(selectionPath, "utf8");
  assert.match(selection, /stopReason === "length".*!params\.hasAssistantVisibleText/);
  const openAi = fs.readFileSync(openAiPath, "utf8");
  assert.match(openAi, /finish_reason: stopReason === "length" \? "length" : "stop"/);
  assert.match(openAi, /requestFinalize\(stopReason === "length" \? "length" : "stop"\)/);

  assert.throws(() => patchOpenClaw(root), /no longer matches/);
});
