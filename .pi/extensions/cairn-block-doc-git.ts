/**
 * Cairn R1: project documentation must not enter git (local commit or remote).
 * Discuss plans and research as full GitHub Issue bodies instead.
 */
import { existsSync } from "node:fs";
import { join } from "node:path";
import { isToolCallEventType, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

const REASON =
	"Cairn R1: 项目文档不得 git add / commit / 推进 PR。方案和调研把全文写入 GitHub Issue。法律材料（legal/、vendor/、LICENSE）除外。";

function isCairnRepo(cwd: string): boolean {
	return existsSync(join(cwd, ".agents/skills/manage-cairn-project/SKILL.md"));
}

function normalize(path: string): string {
	return path.replaceAll("\\", "/").replace(/^\.\//, "");
}

function isForbiddenDoc(path: string): boolean {
	const name = normalize(path);
	if (name.startsWith("vendor/") || name.startsWith("legal/")) return false;
	if (name === "LICENSE" || /(^|\/)LICENSE(\.|$)/.test(name)) return false;
	if (name === "NOTICE" || name.endsWith("/NOTICE")) return false;
	if (name.startsWith("internal/service/translator/dictionary/")) return false;
	if (name.startsWith("docs/") || name.startsWith(".agents/") || name.startsWith("artifacts/")) {
		return true;
	}
	if (name === "README.md") return true;
	return /\.(md|mdx)$/i.test(name);
}

function isGitDocCommand(command: string): boolean {
	if (/\bgh\s+pr\s+create\b/.test(command)) return true;
	if (!/\bgit\s+/.test(command)) return false;
	return /\b(add|commit|rm|push)\b/.test(command);
}

function isBroadAdd(command: string): boolean {
	return /\bgit\s+[^\n]*\badd\b/.test(command) && /(?:\s-A\b|\s--all\b|\s\.\s*$)/.test(command);
}

async function listedFiles(pi: ExtensionAPI, args: string[]): Promise<string[]> {
	const { stdout, code } = await pi.exec("git", args, { timeout: 8000 });
	if (code !== 0) return [];
	return stdout
		.split("\n")
		.map((line) => line.trim())
		.filter(Boolean);
}

async function forbiddenFromGit(pi: ExtensionAPI, command: string): Promise<string[]> {
	const staged = await listedFiles(pi, ["diff", "--cached", "--name-only", "--diff-filter=ACMR"]);
	let candidates = staged;
	if (isBroadAdd(command)) {
		const work = await listedFiles(pi, ["status", "--porcelain", "-u"]);
		const paths = work.map((line) => line.slice(3).trim()).filter(Boolean);
		candidates = [...new Set([...candidates, ...paths])];
	}
	if (/\bgh\s+pr\s+create\b/.test(command) || /\bgit\s+[^\n]*\bpush\b/.test(command)) {
		const vsMain = await listedFiles(pi, ["diff", "origin/main...HEAD", "--name-only"]);
		candidates = [...new Set([...candidates, ...vsMain])];
	}
	const mentioned = [...command.matchAll(/[^\s;'"]+\.(?:md|mdx)\b/gi)].map((match) => match[0]);
	candidates = [...new Set([...candidates, ...mentioned])];
	return candidates.filter((path) => isForbiddenDoc(path));
}

export default function (pi: ExtensionAPI) {
	pi.on("tool_call", async (event, ctx) => {
		if (!isCairnRepo(ctx.cwd)) return;
		if (!isToolCallEventType("bash", event)) return;
		const command = event.input.command ?? "";
		if (!isGitDocCommand(command)) return;
		const hits = await forbiddenFromGit(pi, command);
		if (hits.length === 0) return;
		return {
			block: true,
			reason: `${REASON} 拦住的路径：${hits.join(", ")}`,
		};
	});
}
