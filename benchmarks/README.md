# ASR Spoken-Reference Benchmark

Measures how many **spoken scripture references survive transcription and
detection** — the accuracy metric that decides which ASR engine backs the
Live Detection workspace, where detection is a suggestion only and the
operator always confirms.

The pipeline: recording → 16 kHz mono conversion → engine sidecar → transcript
→ detection → matched against gold references in `manifest.json`.

Each detection type is scored as its own **lane** — explicit references always,
contextual references when a clip declares an `established` passage, quotations
when a translation catalog is pointed at — so every type gets its own recall,
band accuracy, threshold sweep and recommendation, and can be cleared against
the PRD's precision bar before it is enabled in the app.

## Layout

```
benchmarks/
  fetch.ps1        downloads whisper.cpp CPU binaries + tiny.en model
  make-tts-set.ps1 generates a synthetic set covering all four detection
                   types (Windows SAPI voices)
  sample-set/      detector-only demo set (no audio required)
  smoke-set/       engine smoke test (SAPI-synthesized speech + tone)
  tts-set/         generated set: explicit, contextual, quote, correction
  models/          downloaded .ggml/.onnx models          [gitignored]
  tools/           downloaded engine binaries            [gitignored]
  dumps/           -dump transcripts for debugging       [gitignored]
  sermon-set/      create this with YOUR real recordings (see below)
```

`tools/` and `models/` are machine-local. Everything else stays in git.

## Quick start

```powershell
task asr:bench:fetch     # whisper.cpp (CPU x64) + ggml-tiny.en.bin (~75 MB)
task asr:bench           # detector-only demo: no models, no audio
task asr:bench:whisper   # full engine smoke test
powershell -File benchmarks/make-tts-set.ps1   # regenerate tts-set
```

Everything is pure Go except the sidecar binaries, so the same commands work
on Windows, macOS and Linux — only `fetch.ps1` (and its per-OS asset choice)
is Windows-specific today. Release asset names on the whisper.cpp nightly
builds follow `whisper-bin-<os>-<arch>` conventions. `make-tts-set.ps1` is
also Windows-bound (SAPI); on other platforms use edge-tts or piper to
synthesize the same scripts.

## Building a real set

The calibration question — do the shipped confidence bands hold up on real
speech? — needs audio from an actual speaker in the room, which the synthetic
`tts-set` cannot answer. `record-set.ps1` records it: it reads a spoken script,
records one clip per line from the mic, and writes the `manifest.json` the
sweep consumes.

```powershell
powershell -File benchmarks/record-set.ps1 -ListDevices
powershell -File benchmarks/record-set.ps1 -PlanOnly   # check the labels, record nothing
powershell -File benchmarks/record-set.ps1 -Device "Microphone Array (Realtek High Definition Audio)"
go run ./cmd/asr-bench -set benchmarks/sermon-set
  -engine "whisper.tiny=benchmarks/tools/Release/whisper-cli.exe -m benchmarks/models/ggml-tiny.en.bin -np -f {audio}"
```

- The script is `benchmarks/calibration-lines.txt`: one clip per line as
  `<written references> :: <sentence to read aloud> [:: <type> [established=<ref>]]`,
  empty references for a clip that must yield nothing. The lines cover all
  four detection types — explicit calls, word-for-word quotations, bare verse
  calls against an established passage, and corrections — each with a trap
  clip, and deliberately vary the spoken form (chapter/verse in words, "turn
  with me to", bare numbers, ranges, two references in one breath) because
  that is what the scorer has to handle. `-PlanOnly` prints the planned clips
  and their lanes without recording, so the labels can be checked first.
- Default output is `benchmarks/sermon-set/`, which is gitignored: real
  recordings and their manifest stay on the machine that made them.
- `-Seconds` (default 8) sets clip length; `-Countdown` gives the speaker time
  to settle. Re-running appends to the manifest and replaces a clip whose id
  repeats, so a failed line can be redone alone.
- Record in the room you care about, at the distance the mic will actually
  sit, with the band and the PA on if they will be on. A quiet room alone
  calibrates nothing about speech; ten to fifteen short clips is enough to
  read the sweep.

Clips collected another way just need the same manifest; save the WAVs beside
it under `benchmarks/sermon-set/`:

```json
{
  "name": "sunday-2026-10",
  "clips": [
    {
      "id": "opening-call",
      "file": "call-to-worship.wav",
      "references": ["Romans 8:28"],
      "transcript": "optional gold text for WER scoring"
    },
    {
      "id": "verse-mention",
      "file": "verse-twenty-nine.wav",
      "type": "contextual",
      "established": "Romans 8",
      "references": ["Romans 8:29"],
      "transcript": "and verse twenty nine is the promise we hold onto"
    },
    {
      "id": "shepherd-quote",
      "file": "shepherd.wav",
      "type": "quote",
      "references": ["Psalms 23:1-2"],
      "transcript": "the Lord is my shepherd; I shall not want"
    },
    {
      "id": "romans-correction",
      "file": "correction.wav",
      "type": "correction",
      "references": ["Romans 8:29"],
      "transcript": "Romans eight twenty eight. No wait, I mean Romans eight twenty nine."
    }
  ]
}
```

- `references` — gold truth; chapter-level hits count, verse-exact is
  reported separately (ranges legitimately widen).
- `type` — the lane the clip exercises: `explicit` (the default),
  `contextual`, `quote` or `correction`. A lane only scores its own clips, so
  a bare-verse or quotation hearing is never counted as a miss against the
  explicit detector that deliberately ignores it. Gold references are the
  truth for that clip's type — for a correction, the passage the queue should
  keep after the supersede.
- `transcript` — optional; makes WER appear in the report.
- `established` — the passage already on the wall when a contextual clip was
  spoken; its bare "verse N" mentions resolve against it.
- No `file` + a `transcript` = detector-only clip (no audio needed).

Any WAV encoding the engine ecosystem produces (8/16/24/32-bit PCM, 32/64-bit
float, any channel layout) converts automatically to 16 kHz mono first.

## Engine specs

```
-engine name=binary args... {audio}
```

`{audio}` is replaced by the prepared clip path. Quote arguments containing
spaces. Examples:

```powershell
# whisper.cpp, Windows (CPU)
-engine "whisper.tiny=benchmarks/tools/Release/whisper-cli.exe -m benchmarks/models/ggml-tiny.en.bin -np -f {audio}"

# same engine, macOS/Linux
-engine "whisper.tiny=whisper-cli -m models/ggml-tiny.en.bin -np -f {audio}"

# Moonshine tiny (int8) via sherpa-onnx — one runtime also runs Parakeet
$SHERPA = "benchmarks/tools/sherpa-onnx-v2.0.1-win-x64-shared-MT-Release/bin"   # Windows
# $SHERPA = "benchmarks/tools/sherpa-onnx-osx-arm64-shared/bin"                # macOS (adjust tarball)
-engine "moonshine.tiny=$SHERPA/sherpa-onnx-offline.exe --moonshine-preprocessor=benchmarks/models/moonshine-tiny-en-int8/preprocess.onnx --moonshine-encoder=benchmarks/models/moonshine-tiny-en-int8/encode.int8.onnx --moonshine-uncached-decoder=benchmarks/models/moonshine-tiny-en-int8/uncached_decode.int8.onnx --moonshine-cached-decoder=benchmarks/models/moonshine-tiny-en-int8/cached_decode.int8.onnx --tokens=benchmarks/models/moonshine-tiny-en-int8/tokens.txt {audio}"

# Parakeet TDT 0.6B v3 (int8) via the same sherpa-onnx runtime
-engine "parakeet.v3=$SHERPA/sherpa-onnx-offline.exe --encoder=benchmarks/models/parakeet-tdt-0.6b-v3-int8/encoder.int8.onnx --decoder=benchmarks/models/parakeet-tdt-0.6b-v3-int8/decoder.int8.onnx --joiner=benchmarks/models/parakeet-tdt-0.6b-v3-int8/joiner.int8.onnx --tokens=benchmarks/models/parakeet-tdt-0.6b-v3-int8/tokens.txt {audio}"
```

Multiple `-engine` flags run in one pass for a comparison table. Whisper
segment stamps and sherpa-onnx JSON result lines are stripped to plain text
before scoring; engine log noise on stdout is removed automatically.

## Detection lanes

Each clip declares the type it exercises (`type`, default `explicit`), and a
lane only scores its own clips — gold references are per-type truth, not a
shared pool every detector must find.

- **explicit** — always on; `asr.DetectReferences` on the transcript.
- **contextual** — on when any clip is labeled `contextual`; its bare verse
  mentions resolve against the clip's `established` passage. Confidence is the
  app's contextual score, so the sweep shows whether it can ever clear the
  projection bar.
- **correction** — on when any clip is labeled `correction`; a cue phrase tags
  the hearings and the queue supersedes the earlier card, exactly as the app
  does. The lane scores what survives the fold, because that is what the
  operator would see — not the raw pair of hearings.
- **quote** — on when both flags point at an installed catalog:

```powershell
go run ./cmd/asr-bench -set benchmarks/sermon-set `
  -core-db "$env:LOCALAPPDATA\Zowesoft\Zopresent\zopresent.db" `
  -translation-dir "$env:LOCALAPPDATA\Zowesoft\Zopresent\translations" `
  -translation kjv
```

The quote lane matches each transcript against the translation with the same
`MatchQuote` + `DetectQuotes` path the live pipeline uses, so its scores are
the real quote confidences, not a benchmark approximation.

## Metrics

- **recall** — spoken references detected / gold references (the number that
  matters most: a missed reference is a missed opportunity to serve the
  speaker).
- **precision** — of detected references, how many were actually spoken
  (false positives mean the operator sees junk candidates).
- **verse-exact** — detected reference resolved the exact verseStart.
- **WER** — word error rate when gold text exists (context: transcriber
  quality; WER alone does not predict detection success — `Romans 8` from a
  mangled `Romans 8:28` still projects the right chapter).

## Confidence calibration

Every detection is scored with the confidence the app would really show —
`internal/confidence.Evaluate` for explicit and correction phrasings, the
detection's own score for contextual and quote hits — so the report describes
the shipped bands rather than a benchmark copy. Each lane prints:

- **confidence** — detections per band, and how many of each were real. Errors
  clustering in `high` mean the band edges are too generous.
- **threshold sweep** — for each candidate threshold, how many detections it
  would keep (`kept`), how many were real (`correct`), how many were false
  (`false`), and the resulting precision and recall. Recall counts only real
  references, so it is capped by the engine's own misses.
- **recommended** — the answer, not the table: the lowest threshold whose
  precision still meets the PRD's 99% automatic-projection bar, with the recall
  that costs. When no threshold reaches 99% it prints the best precision
  available and says the bar was not met, so a set cannot quietly look ready.
  Read it with `kept` beside it — on a handful of detections a perfect score is
  not evidence.

The grid includes the PRD's starting edges (0.65 medium, 0.92 high) and the
scores the scorer can produce (0.96 written form, 0.93 spoken connectors, 0.86
bare numbers, 0.72 chapter only). Move a band edge to the threshold whose row
holds the precision the operator needs while keeping recall as high as possible;
`models/` and `tools/` stay machine-local, so run this on real sanctuary audio
before changing the constants in `internal/confidence`.

Detector-only mode (`task asr:bench`) exercises the same sweep without any
model or audio, which is how the phrasing scores are checked against gold
transcripts.

## Current model tiers

| Tier | Engine | Models | Disk (models only) |
|------|--------|--------|--------------------|
| low | moonshine.tiny (sherpa-onnx, int8) | `moonshine-tiny-en-int8/` | ~124 MB |
| default | whisper.tiny (whisper.cpp) | `ggml-tiny.en.bin` | ~75 MB |
| high | parakeet.v3 (sherpa-onnx, int8) | `parakeet-tdt-0.6b-v3-int8/` | ~671 MB |

Notes:
- Any single bundle stays below an 800 MB disk budget; the three tiers
  together cost ~870 MB, so models can be deleted after benchmarking.
- **Moonshine degenerates on long inputs**: beyond roughly 20–30 s its decoder
  can fall into a repetition loop ("I'm a little bit more than a," ×400),
  pinning the CPU and growing memory until a small machine thrashes. Keep it
  to short chunks (the live pipeline analyzes ~20 s); use whisper.cpp or
  Parakeet for whole-clip transcription.
- `whisper.cpp` also has `small.en` (~466 MB) as a middle option via the same
  CLI — add the model file if you want it scored.
- Parakeet v3 is multilingual (25 European languages) and reports ~6.3% WER
  on the HF leaderboard; Moonshine targets latency over accuracy; whisper
  `tiny.en` is the accuracy floor for smoke-testing the pipeline.
- The sherpa-onnx runtime (~90 MB unpacked, per OS) sits in `tools/`, not the
  model budget, but counts toward your disk: total toolchain ≈ 1 GB with all
  tiers installed.

The whole point of this harness: **benchmark those on real sanctuary audio
through the actual detector before committing.**

