#!/usr/bin/env node
// Narration with Piper (local TTS). For a chapter, one wav per step + the intro,
// written to out/training/<chapter>/audio/, plus durations in narration.json.
//   node narrate.mjs ch01

import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const id = process.argv[2] || "ch01";
const ch = (await import(`./chapters/${id}.mjs`)).default;
const DIR = new URL(`./out/training/${id}/audio/`, import.meta.url).pathname;
mkdirSync(DIR, { recursive: true });
const PIPER = new URL("./bin/piper/piper", import.meta.url).pathname;
const VOICE = new URL(`./voices/${process.env.VOICE || "en_US-lessac-medium"}.onnx`, import.meta.url).pathname;

const say = (name, text) => {
  const wav = `${DIR}${name}.wav`;
  // small pause at the end so captions don't cut the last word
  execFileSync(PIPER, ["--model", VOICE, "--output_file", wav, "--sentence_silence", "0.35", "--length_scale", "1.05"], { input: text, stdio: ["pipe", "ignore", "ignore"] });
  const dur = parseFloat(execFileSync("ffprobe", ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", wav]).toString());
  return { file: `${name}.wav`, dur };
};

const out = { intro: say("intro", ch.intro), steps: {} };
for (const st of ch.steps) out.steps[st.id] = say(st.id, st.text);
writeFileSync(`${DIR}../narration.json`, JSON.stringify(out, null, 2));
console.log(`${id}: intro ${out.intro.dur.toFixed(1)}s, ` + Object.entries(out.steps).map(([k, v]) => `${k} ${v.dur.toFixed(1)}s`).join(", "));
