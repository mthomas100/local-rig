// web-research-rules.ts — the few web-research rules that must always be in
// front of the local model, appended to the system prompt whenever the
// web_search tool (pi-web-access) is active.
//
// Why not only the web-research skill: pi shows a skill's description in the
// prompt and loads its body only if the model chooses to read it. In the
// 2026-09-23 benchmark the local model went straight to web_search without
// reading the skill, then fetched a 20000-character GitHub API document where
// one `gh api` call would have answered. The skill keeps the full procedure;
// these lines are the part worth a permanent ~300 tokens. Measured effect on
// the same eight benchmark questions: GitHub questions go straight to `gh api`,
// and no run with these rules or the skill sent a URL through a public proxy
// (the run with neither did, five times, on Reddit). Total time was within
// run-to-run noise, so the case for them is behaviour, not speed.
//
// Sites that wall anonymous readers (Reddit, X, some pricing pages) are not
// worked around here: the rule is to use an official API where one exists,
// and otherwise to say the page could not be read.
//
// The text is constant, so it does not disturb the model server's prompt
// prefix cache. Symlinked into ~/.pi/agent/extensions/.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const RULES = `
## Web research rules
- Your memory is out of date: look up anything that changes over time and cite it.
- If a structured API answers it, use bash: \`gh api\` for GitHub; registry.npmjs.org and api.npmjs.org for npm; pypi.org/pypi/<pkg>/json; en.wikipedia.org/api/rest_v1; hn.algolia.com/api/v1. Filter JSON with jq; never print a whole document.
- Otherwise: web_search with one or two short queries per call, then fetch_content on the one to three best primary sources. For a long or truncated page, use get_search_content with findText.
- Never curl a web page into the conversation. A page that answers with a block page, a JavaScript shell, HTTP 403 or a login wall: use the site's API if it has one, otherwise say it could not be read; never public proxies, mirrors or reader services.
- If web_search fails, run ~/repos/local-rig/tools/searxng-health.sh and report what it says.
- Answer first, then "Sources:" with each URL, what it confirmed and its date. Mark anything unverified. If sources do not show something the user named, say it was not found; do not describe it.
- For a long multi-source report, use the deep-research skill. The web-research skill has the full procedure.`;

export default function (pi: ExtensionAPI) {
  pi.on("before_agent_start", async (event) => {
    const tools = event.systemPromptOptions?.selectedTools ?? [];
    if (!tools.includes("web_search")) return;
    return { systemPrompt: `${event.systemPrompt}\n${RULES}` };
  });
}
