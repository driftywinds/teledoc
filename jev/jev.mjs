#!/usr/bin/env node
// jev.mjs — send one System One (Jev) request and print a structured summary.
//
// Usage:
//   node jev/jev.mjs <request.json>     request file: { "state": ..., "model"?: ..., "questions": { id: { type, instructions, criteria? } } }
//   node jev/jev.mjs -                  read the request JSON from stdin
//
// The API key is read from the TYPESAFE_API_KEY environment variable — it is
// NEVER stored in this file or in any request file. Set it once with:
//   setx TYPESAFE_API_KEY "<your key>"     (in your own terminal, then restart it)
//
// Docs: https://docs.typesafe.ai/api  |  Pricing: $42 per billion INPUT tokens,
// output tokens free (https://docs.typesafe.ai/models).
import { readFileSync } from 'node:fs';
import { performance } from 'node:perf_hooks';

const ENDPOINT = 'https://api.typesafe.ai/v1/systemone';
const PRICE_PER_BTOK_USD = 42; // per billion input tokens; output tokens are free

function readInput() {
  const arg = process.argv[2];
  if (arg) return readFileSync(arg, 'utf8');
  return readFileSync(0, 'utf8'); // stdin
}

const key = process.env.TYPESAFE_API_KEY;
if (!key) {
  console.error('TYPESAFE_API_KEY is not set in this environment.');
  console.error('Fix: run  setx TYPESAFE_API_KEY "<your key>"  in your own terminal, then reopen it.');
  process.exit(2);
}

let req;
try {
  req = JSON.parse(readInput());
} catch (e) {
  console.error(`Invalid JSON input: ${e.message}`);
  process.exit(2);
}
if (!req.model) req.model = 'jev-latest';

const t0 = performance.now();
let res;
try {
  res = await fetch(ENDPOINT, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
} catch (e) {
  console.error(`Request failed before reaching the API: ${e.cause?.message ?? e.message}`);
  process.exit(1);
}
const ms = Math.round(performance.now() - t0);

const text = await res.text();
console.log(`HTTP ${res.status} ${res.statusText} - ${ms} ms round trip`);
if (!res.ok) {
  console.log('ERROR BODY:');
  console.log(text);
  process.exit(1);
}

const data = JSON.parse(text);
console.log(`model: ${data.model}`);
console.log('');
for (const [id, a] of Object.entries(data.answers ?? {})) {
  if (a.type === 'choice') {
    console.log(`${id} (choice) -> ${a.choice}   confidence=${a.confidence}`);
    for (const [opt, p] of Object.entries(a.probabilities ?? {})) console.log(`   ${opt}: ${p}`);
  } else if (a.type === 'score') {
    console.log(`${id} (score) -> ${a.score}   confidence=${a.confidence}`);
    for (const [lvl, desc] of Object.entries(a.legend ?? {})) {
      console.log(`   level ${lvl}: ${desc}   p=${a.probabilities?.[lvl]}`);
    }
  } else if (a.type === 'noul') {
    console.log(`${id} (noul) -> ${a.noul}   (probability of yes)`);
  } else {
    console.log(`${id}: ${JSON.stringify(a)}`);
  }
}
console.log('');
const u = data.usage ?? {};
const cost = (u.input_tokens ?? 0) * PRICE_PER_BTOK_USD / 1e9;
console.log(`usage: input_tokens=${u.input_tokens} output_tokens=${u.output_tokens} (output tokens are free)`);
console.log(`cost:  ${u.input_tokens} input tokens x $42/Btok = $${cost.toFixed(8)}  (~${(cost * 100).toFixed(5)} cents)`);