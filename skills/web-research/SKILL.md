---
name: web-research
description: "Look things up on the internet and answer with cited, dated sources. Use when the answer depends on information that is current, changing, or outside your training data: latest versions, release dates, prices, plan limits, news, package/API syntax, error messages in tools and CLIs, CVE/security advisories, and before recommending a library, tool, API or model. Also whenever the user asks you to look something up, search, check online or research. Do NOT use for stable coding concepts you already know, or for this machine's local state (use bash/lsof). For a long multi-source report, use the deep-research skill instead."
---

# Web research

Your training data is older than today's date, which is in your system prompt.
For anything that changes over time, the internet is the source of truth and
your memory is only a hint about where to look.

Reach for this skill when you hit an error or an unknown config in a tool, CLI,
library or API you have not read the docs for: the answer is usually documented
and searchable, so look it up before reverse-engineering it locally. Keep local
debugging (bash, lsof, pgrep) for this machine's own state, which no web page
can tell you.

## Pick the cheapest source that can answer

1. **A structured API, through bash.** Small JSON answers, no page noise.
   - GitHub: `gh api repos/OWNER/REPO/releases/latest`, `gh api repos/OWNER/REPO`,
     `gh search repos "<words>" --limit 10`
   - npm: `curl -s https://registry.npmjs.org/<pkg>` and
     `curl -s https://api.npmjs.org/downloads/point/last-week/<pkg>`
   - PyPI: `curl -s https://pypi.org/pypi/<pkg>/json`
   - Wikipedia: `curl -s https://en.wikipedia.org/api/rest_v1/page/summary/<Title>`
   - Hacker News: `curl -s "https://hn.algolia.com/api/v1/search?query=<words>"`
   Filter with `jq` or `python3 -c`; never print a whole JSON document.
2. **`web_search`** when you do not know where the answer lives. It queries the
   local SearXNG. Send one or two short queries per call, with the year for
   anything "latest". More queries per call burn the search engines' rate
   limits for no gain; search again only if the first results are thin.
3. **`fetch_content`** on the one to three best URLs from the results. Prefer
   the primary source: the project's own site, docs, release page or vendor
   pricing page, over blogs and aggregators.
4. **`get_search_content` with `findText`** when a fetched page was truncated or
   is long (a Wikipedia article, a changelog). Search the stored copy for the
   words you need instead of reading it all again.

Never `curl` a web page into the conversation. Raw HTML costs ten to a hundred
times more context than the readable text `fetch_content` returns.

## Sites that block automated readers

Some sites (social networks, npmjs.com package pages, some pricing pages)
return a block page, a CAPTCHA or an empty JavaScript shell. When that happens:

- Use the site's API if one of the list above covers it (npm downloads, for
  example).
- Do not retry the same page with `curl`, `fetch_content` or a fresh browser:
  the wall is the answer.
- Do not route URLs through public proxies, mirrors or reader services
  (r.jina.ai, allorigins and the like). They leak what the user is reading
  to third parties and they rarely work.
- If none of that works, say the page could not be read and answer only what
  other sources confirm.

## When search itself fails

If `web_search` returns an error, run `~/repos/local-rig/tools/searxng-health.sh`
and tell the user what it says (DOWN or DEGRADED names the failing engines).
Carry on with the APIs above where they cover the question. Never fill the gap
from memory without saying so.

## Checking and stopping

- A claim is verified when a primary source states it, or when two independent
  sources agree.
- Something the user names may not exist (a version, a paper, a feature). If
  sources do not show it, say so plainly rather than describing it.
- Stop as soon as every part of the question is verified.

## Answer format

Answer first, in one or two sentences, with exact numbers, versions and dates
as the source states them. Then a `Sources:` list, one line per source: the
URL, what it confirmed, and its date if it shows one. Mark anything you could
not verify as **unverified** and say why.
