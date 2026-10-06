// today.ts — put today's date in pi's system prompt.
//
// pi's built-in prompt carries the working directory but no date, so a local
// model cannot tell that what it remembers as "the latest" release, price or
// news is out of date. In the 2026-09-23 web-research baseline the model ran
// `date` on its own in some tasks and not in others.
//
// Date only, no time of day: the system prompt is the head of every request,
// and a value that changed on every call would invalidate the model server's
// prompt-prefix KV cache for the whole conversation. A date changes once a day.
//
// Symlinked into ~/.pi/agent/extensions/ (global, no project-trust prompt).
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

function today(now = new Date()): string {
  const iso = [
    now.getFullYear(),
    String(now.getMonth() + 1).padStart(2, "0"),
    String(now.getDate()).padStart(2, "0"),
  ].join("-");
  const weekday = now.toLocaleDateString("en-US", { weekday: "long" });
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  return `${weekday} ${iso} (${zone})`;
}

export default function (pi: ExtensionAPI) {
  pi.on("before_agent_start", async (event) => ({
    systemPrompt:
      `${event.systemPrompt}\n\nToday's date: ${today()}. Your training data ` +
      "is older than this, so for anything that changes over time (versions, " +
      "releases, prices, plan limits, news) check a current source rather than " +
      "answering from memory.",
  }));
}
