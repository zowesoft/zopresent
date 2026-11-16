# Zopresent Benchmarks

Two harnesses live here:

- **ASR spoken-reference benchmark** (`cmd/asr-bench`) — how many spoken
  scripture references survive transcription and detection; decides which
  engine backs Live Detection and which detection types may be enabled.
- **Semantic search benchmark** (`cmd/search-bench`) — local embedding models
  against the FTS baseline on a labeled query set; the measurement the PRD
  requires before F08 selects a model and vector storage. See
  [Semantic search benchmark](#semantic-search-benchmark-f08) below.

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
  fetch.ps1        downloads whisper.cpp, sherpa-onnx, llama.cpp CPU binaries
                   and their models
  make-tts-set.ps1 generates a synthetic set covering all four detection
                   types (Windows SAPI voices)
  sample-set/      detector-only demo set (no audio required)
  smoke-set/       engine smoke test (SAPI-synthesized speech + tone)
  tts-set/         generated set: explicit, contextual, quote, correction
  search-set/      labeled semantic/keyword/trap queries for search-bench
  models/          downloaded .ggml/.onnx/.gguf models     [gitignored]
  tools/           downloaded engine binaries            [gitignored]
  dumps/           -dump transcripts, sidecar logs, vector caches [gitignored]
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

# Semantic search benchmark (F08)

`cmd/search-bench` answers the question the tracker gates F08 on: *which local
embedding model, if any, earns its place on the supported Windows baseline?*
It embeds the installed translation's verses plus the song catalog through a
`llama-server` sidecar, ranks a labeled query set by cosine similarity, and
scores the same queries through the production FTS path. The report carries
the four numbers the PRD demands before a model is selected: retrieval
relevance, indexing cost, memory use and latency.

The harness is pure Go (`CGO_ENABLED=0`); only the sidecar binary is per-OS.
llama.cpp ships CPU builds for Windows, macOS (arm64/x64) and Linux, so the
same command shape runs on every supported platform.

## Running it

```powershell
powershell -File benchmarks/fetch.ps1   # also fetches llama.cpp + 3 GGUF models

go run ./cmd/search-bench -set benchmarks/search-set `
  -core-db "$env:LOCALAPPDATA\Zowesoft\Zopresent\zopresent.db" `
  -translation-dir "$env:LOCALAPPDATA\Zowesoft\Zopresent\translations" `
  -translation kjv `
  -engine "minilm-q8=benchmarks/tools/llama-b12436/llama-server.exe -m benchmarks/models/all-MiniLM-L6-v2-Q8_0.gguf --embeddings --port {port} -c 8192 -b 8192 -ub 2048 --parallel 4 -t 4" `
  -cache benchmarks/dumps/search-cache -out benchmarks/search-report-minilm.json
```

- `{port}` is replaced with a free loopback port; the harness waits for
  `/health` before indexing. Sidecar logs land in `benchmarks/dumps/search/`.
- `-engine` is repeatable — several models score in one pass against the same
  FTS baseline. With no `-engine` flags only the baseline runs.
- `-cache` persists vectors keyed by model identity + corpus content, so
  repeat runs report `warm` load time instead of re-embedding; cold indexing
  cost is always measured at least once and never hidden.
- `-t 4` in the spec models a modest machine; raise it to measure a bigger
  one. `-batch`, `-runs` and `-depth` tune batch size, timed repetitions and
  FTS retrieval depth.
- Model task prefixes are harness tokens, not server flags: nomic wants
  `query-prefix=search_query: doc-prefix=search_document:`, bge-small wants
  `query-prefix="Represent this sentence for searching relevant passages: "`.
  MiniLM needs none.

## The query set

`search-set/manifest.json` labels every query with a type and gold truth:

- **semantic** — meaning-based paraphrases with (near) zero content-word
  overlap with the gold verse; the harness measures and reports the actual
  overlap so the set cannot quietly drift into keyword matching. Includes
  testament- and book-filtered queries.
- **keyword** — exact or near-exact phrases the FTS path should already win.
- **trap** — proverbs that are not scripture and announcement-style noise;
  gold is empty, and the report shows what each lane returns anyway.
- **domain: song** — queries against the installed song catalog (titles and
  lyric sections), gold by song title.

Gold references may be ranges; a hit is the first ranked document inside any
gold range. Recall@1/5/10 and MRR are reported per type, per domain. Embedding
lanes rank the whole corpus (exact relevance, no ANN approximation); the FTS
lane ranks its retrieved depth (default 50).

## Reading the report

- **semantic recall@10** is the headline: what fraction of meaning-based
  queries surface their passage in the first ten results. The FTS baseline
  sits near zero by construction — that gap is the feature's reason to exist.
- **keyword recall@10** guards against regression: semantic search must not
  lose the exact-phrase searches the app already answers well.
- **query p50/p95** includes embedding the query plus ranking, per type.
  Compare against the same 150 ms search budget `cmd/bible-bench` holds the
  FTS path to.
- **index** reports docs/s, vector footprint, model size and the sidecar's
  peak resident set while indexing and while querying — the PRD baseline is
  a limited-RAM Windows machine running semantic indexing alongside media
  and Whisper, so memory is a selection criterion, not a footnote.
- **traps** show the top similarity a non-scripture query reaches; that
  separation is what a future "no confident match" cutoff would sit on.
  Similarity is a ranking score, never a detection confidence, and nothing
  here auto-projects.

This slice measures and recommends; it does not build the feature. No
`search_embeddings` table, no app wiring — those wait for the model decision
this report informs.

## Windows baseline results (2026-11-16)

Machine: Intel i5-4300U (2 cores / 4 threads, 1.9 GHz), 8 GB RAM, Windows 11 —
the PRD's limited-hardware baseline, with the app itself running alongside.
Corpus: KJV, 31,102 verses + 7 song documents. Query set: 22 semantic
paraphrases (measured content-word overlap with gold: 1.9%), 6 keyword,
3 traps, 6 song queries. Full numbers in `search-report.json`; per-query
ranks with `-v`.

| Lane | Semantic recall@10 | Keyword recall@10 | Songs | Query p95 | Cold index | Sidecar RSS |
|---|---|---|---|---|---|---|
| FTS (production) | 0% (0/22) | 100% | 2/6 | 544 ms semantic / 68 ms keyword | — | — |
| minilm-q8 | 22.7% (5/22) | 33% | 6/6 rank 1 | 78 ms | ~21 min (24.6 docs/s) | 32–36 MB |
| bge-q8 | 18.2% (4/22) | 100% | 6/6 rank 1 | 199 ms | ~46 min (11.4 docs/s) | 44–48 MB |
| nomic-q8 | not scored | — | — | — | ~4 h projected (2.17 docs/s) | 168 MB at 16% |

Cold index times come from the first pass's sidecar logs (completed-task
counts match the corpus exactly); the tracked report re-ran from the warm
vector cache, which loads in ~200–450 ms. Models are Q8_0 GGUF: 23.8 / 35.1 /
146 MB; vectors 45.6 MB per 384-dim model, 91 MB for nomic's 768.

**Recommendation: all-MiniLM-L6-v2 (Q8_0), strictly hybrid with FTS.**

- FTS stays the primary path: it owns keyword and phrase search (100%,
  ~3 ms p50) and MiniLM alone *regresses* it ("still waters" lands at rank
  156). The semantic lane supplements, never replaces.
- MiniLM wins the semantic lane on this baseline: best recall@10 (and 12/22
  within rank 50 vs bge's 6/22), query p95 inside the 150 ms search budget,
  half bge's index cost, smallest model and footprint.
- bge-small is the runner-up: perfect keyword behaviour and higher gold
  similarity, but its score distribution is compressed (non-matches score
  almost as high as matches), its semantic recall is lower on this set, and
  its p95 misses the budget on baseline hardware.
- nomic is disqualified on install cost: a ~4-hour first index and growing
  168 MB+ RSS on the baseline machine, for a context window (8,192 tokens)
  that verse- and stanza-level indexing never uses.

**Honest limits of the result:**

- The semantic set is deliberately hard — near-zero word overlap, archaic
  KJV vocabulary, single-verse gold. Real operator queries carry partial
  overlap and will score better; several bge "misses" are adjacent verses
  (John 3:18 for a John 3:16 query) an operator would still accept. Treat
  these numbers as a lower bound, not a ceiling.
- 22 semantic queries is a small sample. Grow the set with real operator
  queries before calibrating the shipped feature.
- Neither model separates traps cleanly (bge's top trap score 0.734 beats
  many true matches). Similarity is a ranking score, never a detection
  confidence; semantic results stay labeled suggestions with no
  auto-projection, and a "no confident match" cutoff cannot be trusted to
  these scores alone.
- Worth measuring next if the semantic lane ships: overlapping two-verse
  windows for multi-verse paraphrases, and reciprocal-rank fusion of the
  FTS and vector rankings.

