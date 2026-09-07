import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const patches = [
  {
    file: "selection-me3nXBxK.js",
    before: `function isIncompleteTerminalAssistantTurn(params) {
	const stopReason = params.lastAssistant?.stopReason;
	return stopReason === "toolUse" || stopReason === "length" && !params.hasTerminalOutput;
}`,
    after: `function isIncompleteTerminalAssistantTurn(params) {
	const stopReason = params.lastAssistant?.stopReason;
	return stopReason === "toolUse" || stopReason === "length" && !params.hasTerminalOutput && !params.hasAssistantVisibleText;
}`,
  },
  {
    file: "openai-http-B9VYVAS4.js",
    before: `					finish_reason: "stop"
`,
    after: `					finish_reason: stopReason === "length" ? "length" : "stop"
`,
  },
  {
    file: "openai-http-B9VYVAS4.js",
    before: `			requestFinalize();
		} catch (err) {
			resultResolved = true;`,
    after: `			requestFinalize(stopReason === "length" ? "length" : "stop");
		} catch (err) {
			resultResolved = true;`,
  },
];

function replaceExactlyOnce(source, before, after, filename) {
  const first = source.indexOf(before);
  if (first === -1) {
    throw new Error(`OpenClaw compatibility patch no longer matches ${filename}`);
  }
  if (source.indexOf(before, first + before.length) !== -1) {
    throw new Error(`OpenClaw compatibility patch matched ${filename} more than once`);
  }
  return source.slice(0, first) + after + source.slice(first + before.length);
}

export function patchOpenClaw(root = "/app") {
  for (const patch of patches) {
    const filename = path.join(root, "dist", patch.file);
    const source = fs.readFileSync(filename, "utf8");
    const updated = replaceExactlyOnce(source, patch.before, patch.after, filename);
    fs.writeFileSync(filename, updated);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  // OpenClaw 2026.7.1 discards visible assistant text when the provider stops
  // at the output-token limit. Upstream fixed that after 2026.8.1-beta.2, but
  // no stable image contains it yet. These exact-bundle checks fail closed when
  // the pinned base changes so this backport cannot silently patch new code.
  patchOpenClaw(process.argv[2]);
}
