---
name: jev
description: >
  Sort and decide things with Jev, the TypeSafe "System One" model: send text
  (state) plus typed questions and get instant structured answers — a choice
  from a list, a score on a scale, or a yes-probability — each with
  confidence. Use when the user says "use Jev" or asks to classify, score,
  rank, triage, route, or make yes/no decisions about any pile of text.
whenToUse: Sorting, classifying, scoring, ranking, routing, or deciding over text with Jev/TypeSafe; lead scoring; triage; yes/no checks; when the user names Jev.
---

# Jev — instant structured decisions (TypeSafe)

**The standing rule: Jev decides, you write.** Jev never writes text, code, or
explanations — it only returns typed answers to the questions you ask. Use it
for sorting and deciding; do all writing yourself. **When Jev isn't sure, you
make the call yourself** (see the certainty policy below).

## The three question shapes (all Jev can answer)

| Need | Type | Answer |
| --- | --- | --- |
| Pick one option from a list | `choice` | `choice`, `probabilities`, `confidence` |
| Score on an ordered scale | `score` | `score` (can land between levels), `legend`, `probabilities`, `confidence` |
| How likely something is true | `noul` | `noul` = probability of yes (0–1). No separate confidence; 0.5 means unsure, not "medium". |

## How to call it

**Helper (preferred):** `node jev/jev.mjs <request.json>` from the repo root —
request file is `{ "state": <text or JSON>, "model": "jev-latest", "questions":
{ <id>: { type, instructions, criteria? } } }`. It prints each answer,
confidence/probabilities, round-trip time, and cost. See `jev/test-request.json`
for a working example (fictional sales email: lead strength + email type +
needs-personal-reply in one call).

- Raw endpoint if needed: `POST https://api.typesafe.ai/v1/systemone`,
  `Authorization: Bearer $TYPESAFE_API_KEY`, JSON body same shape. Answers come
  back under the same ids you chose. Full docs: https://docs.typesafe.ai/api
  (append `.md` to page URLs for markdown; index: https://docs.typesafe.ai/llms.txt).
- **On this machine do NOT call the API from PowerShell/.NET/curl.exe** — the
  sandbox blocks Windows Schannel TLS credentials (`SEC_E_NO_CREDENTIALS`). Use
  node (OpenSSL) or python instead.
- If `$env:TYPESAFE_API_KEY` is empty in a fresh shell, hydrate it from the
  registry: `$env:TYPESAFE_API_KEY = [Environment]::GetEnvironmentVariable('TYPESAFE_API_KEY','User')`.

## Writing good questions

- Batch every question that reads the same state into ONE request — they run in
  parallel, extra questions cost only a few tokens and ~no latency. Include
  speculative ones; ignore answers you don't need.
- IDs are for your code only and are NOT sent to the model — the full meaning
  goes in `instructions`.
- `choice`: `criteria` is a map `{option: description}` (max 255); add an
  `other` option when the list may not cover every input.
- `score`: `criteria` is an ordered array of level descriptions (2–10 levels);
  describe concrete situations per level.
- `noul`: optional `criteria: {true: "...", false: "..."}` to define yes/no.
- One narrow snap judgment per question ("does this convey urgency?"), not
  "analyze this and decide". Split multi-factor judgments into several questions
  and combine in code with your own weights.
- Reference parts of a structured state by backticked paths, e.g. `` `messages[0].text` ``.
- A second request is only needed when a later question depends on an earlier
  answer (new state or new options). Otherwise batch.

## Certainty policy — "when Jev isn't sure, you decide"

- `choice`/`score` confidence ≥ 0.8 → use Jev's answer.
- 0.5–0.8 → proceed, but sanity-check it yourself and note the doubt.
- < 0.5 → ignore the pick; you decide from the raw text.
- `noul` ≥ 0.85 or ≤ 0.15 → act on it; in between → you decide.
Tighten thresholds for high-stakes calls. Always surface Jev's answer +
confidence to the user when it informed a decision.

## Cost, limits, errors

- Pricing: **$42 per BILLION input tokens; output tokens free.**
  `cost = input_tokens * 42 / 1e9` (the helper prints it). A 3-question request
  ≈ 800 tokens ≈ $0.00003 — pennies per thousand.
- Limits: ~250k tokens/s, 1200 req/min; 64k tokens per request (state + all
  questions; 32k for state + longest question). Text only.
- Errors: 401 bad key, 422 validation failure (read the body — a question is
  malformed), 429/529 rate/overload → retry with backoff.

## Key & privacy rules (binding, from the user)

- The key lives ONLY in the user-scope environment variable `TYPESAFE_API_KEY`.
  Never write it to any file, never print it back, never embed it in code or
  configs. If it's missing, ask the user to run in their own terminal:
  `setx TYPESAFE_API_KEY "<their key>"` (then reopen the terminal).
- Anything sent to Jev leaves the user's computer: **ask before sending private
  or confidential text**. Invented/synthetic test data needs no permission.

## Verified live

`node jev/jev.mjs jev/test-request.json` (fictional cold-outreach email) →
HTTP 200 in ~1.5 s, model jev-1.13.0: lead_strength = 2.15/4 "Moderate lead"
(confidence 0.85, 82% on level 2), email_type = cold_outreach (confidence 1.0),
needs_personal_reply = 0.71 probability of yes. Cost $0.00003184 (758 input
tokens).