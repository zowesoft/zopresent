// Command asr-bench transcribes benchmark clips through one or more ASR engine
// sidecars, runs each transcript through every enabled detection type, and
// scores reference recall plus word error rate where gold text is available.
//
// Each detection type (explicit reference, contextual reference, quotation) is
// scored as its own lane with its own confidence sweep and recommendation, so
// each type can be calibrated against real, labeled audio and cleared against
// the PRD's precision bar before it is enabled.
//
// Usage:
//
//	go run ./cmd/asr-bench -set benchmarks/sermon-set ^
//	  -engine whisper-cli=bin\whisper-cli.exe -m models\ggml-small.en.bin -np -f {audio}
//
// With no -engine flags the gold transcripts (or .txt files beside the clips)
// run through the detector alone, isolating the detector from engine behavior.
// A clip may declare an "established" passage to exercise the contextual lane;
// the quote lane activates when -translation-dir and -core-db point at an
// installed translation catalog. See benchmarks/README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zopresent/internal/asr"
	"zopresent/internal/bible"
	"zopresent/internal/confidence"
	"zopresent/internal/database"
	"zopresent/internal/detection"
	"zopresent/internal/scripture"
)

type manifest struct {
	Name  string      `json:"name"`
	Clips []benchClip `json:"clips"`
}

type benchClip struct {
	// ID is the display name for the clip (defaults to the file name or
	// "clip-NN").
	ID string `json:"id"`
	// File is the WAV path relative to the set directory. Optional in
	// detector-only mode.
	File string `json:"file"`
	// References are the gold references spoken in the clip.
	References []string `json:"references"`
	// Established is the passage on the wall when the clip was spoken; a bare
	// "verse N" mention resolves against it for the contextual lane.
	Established string `json:"established,omitempty"`
	// Transcript optionally provides gold text used for WER scoring and for
	// detector-only runs.
	Transcript string `json:"transcript"`
}

type clipResult struct {
	Clip     string   `json:"clip"`
	Expected []string `json:"expected"`
	Detected []string `json:"detected"`
	Hits     []string `json:"hits"`
	Missed   []string `json:"missed"`
	Extra    []string `json:"extra"`
	// Scores are the confidence each detected reference received, in the same
	// order as Detected.
	Scores     []float64 `json:"scores,omitempty"`
	VerseExact int       `json:"verse_hits"`
	Transcript string    `json:"transcript,omitempty"`
	WER        float64   `json:"wer,omitempty"`
}

// laneSummary is one detection type's score over the whole set.
type laneSummary struct {
	Name       string       `json:"name"`
	Clips      []clipResult `json:"clips"`
	Expected   int          `json:"expected"`
	Hits       int          `json:"hits"`
	Extra      int          `json:"extra"`
	VerseExact int          `json:"verse_hits"`
	// Band accuracy and the threshold sweep are what calibrate the confidence
	// bands; the counts are detections, not references.
	HighCorrect   int                `json:"high_correct"`
	HighFalse     int                `json:"high_false"`
	MediumCorrect int                `json:"medium_correct"`
	MediumFalse   int                `json:"medium_false"`
	LowCorrect    int                `json:"low_correct"`
	LowFalse      int                `json:"low_false"`
	Calibration   []confidence.Point `json:"calibration,omitempty"`
	// observations feeds the sweep and is never serialised.
	observations []confidence.Observation
}

type engineSummary struct {
	Engine       string        `json:"engine"`
	Lanes        []laneSummary `json:"lanes"`
	TranscribeMs int64         `json:"transcribe_ms"`
	MeanWER      float64       `json:"mean_wer,omitempty"`
	WERClips     int           `json:"wer_clips"`
}

type engineSummaries struct {
	Engines []engineSummary `json:"engines"`
}

// laneDef names a detection type and produces its detections for one transcript.
type laneDef struct {
	name string
	run  func(i int, text string) []asr.Detection
}

// quoteMatchLimit mirrors the app's per-chunk quote index budget.
const quoteMatchLimit = 32

func main() {
	setDir := flag.String("set", "benchmarks/sermon-set", "directory containing manifest.json and clip audio")
	var engineSpecs multiFlag
	flag.Var(&engineSpecs, "engine", "engine spec name=bin args... {audio}; repeatable")
	dumpTranscripts := flag.String("dump", "", "optional directory to write raw transcripts per engine")
	outJSON := flag.String("out", "", "optional path for a JSON report")
	translationDir := flag.String("translation-dir", "", "translations directory; with -core-db enables the quote lane")
	coreDB := flag.String("core-db", "", "core database holding the translation catalog; enables the quote lane")
	translationID := flag.String("translation", "kjv", "translation id for the quote lane")
	flag.Parse()

	setPath = strings.TrimSuffix(*setDir, "/") // normalise windows slashes naturally
	manifestPath := filepath.Join(*setDir, "manifest.json")
	set, err := readManifest(manifestPath)
	if err != nil {
		exit(err.Error())
	}
	if len(set.Clips) == 0 {
		exit(fmt.Sprintf("%s declares no clips", manifestPath))
	}

	// Audio conversion happens once into a scratch directory, so conversion
	// cost is never attributed to a particular engine.
	workDir, cleanup := scratchDir()
	defer cleanup()

	gold := parseGold(set.Clips)

	var runs []benchmarkRun

	if len(engineSpecs.values) > 0 {
		for _, spec := range engineSpecs.values {
			engine, err := asr.ParseEngineSpec(spec)
			if err != nil {
				exit(err.Error())
			}
			texts, errs, elapsed := transcribeClips(engine, set, workDir, *dumpTranscripts)
			runs = append(runs, benchmarkRun{name: engine.Name(), texts: texts, errs: errs, elapsed: elapsed})
		}
	} else {
		texts, errs := detectorOnlyTranscripts(set)
		if *dumpTranscripts != "" {
			for i, text := range texts {
				if errs[i] == nil {
					writeDump(*dumpTranscripts, "detector-only", clipName(set.Clips[i], i), text)
				}
			}
		}
		runs = append(runs, benchmarkRun{name: "detector-only", texts: texts, errs: errs})
	}

	lanes := buildLanes(set, *translationDir, *coreDB, *translationID)

	var report engineSummaries
	for _, run := range runs {
		report.Engines = append(report.Engines, scoreRun(set, gold, run, lanes))
	}

	printReport(report.Engines)

	if *outJSON != "" {
		if err := writeJSON(*outJSON, report); err != nil {
			exit(fmt.Sprintf("write %s: %v", *outJSON, err))
		}
		fmt.Printf("report written to %s\n", *outJSON)
	}
}

// buildLanes enables the explicit lane always, the contextual lane when a clip
// declares an established passage, and the quote lane when a translation
// catalog is pointed at. Each lane scores the confidence the app would really
// show, so the sweep describes production bands, not a benchmark copy.
func buildLanes(set manifest, translationDir, coreDB, translationID string) []laneDef {
	lanes := []laneDef{
		{name: "explicit", run: func(i int, text string) []asr.Detection { return asr.DetectReferences(text) }},
	}

	hasEstablished := false
	for _, clip := range set.Clips {
		if clip.Established != "" {
			hasEstablished = true
			break
		}
	}
	if hasEstablished {
		established := parseEstablished(set.Clips)
		lanes = append(lanes, laneDef{name: "contextual", run: func(i int, text string) []asr.Detection {
			est := established[i]
			if est.Book.ID <= 0 || est.Chapter <= 0 {
				return nil
			}
			explicit := asr.DetectReferences(text)
			return detection.DetectContextualReferences(text, est, explicit, func(int) bool { return true })
		}})
	}

	if translationDir != "" && coreDB != "" {
		ctx := context.Background()
		core, err := database.OpenReadOnly(ctx, coreDB)
		if err != nil {
			exit(fmt.Sprintf("open core db %s: %v", coreDB, err))
		}
		repo := bible.NewSQLiteRepository(core, translationDir)
		lanes = append(lanes, laneDef{name: "quote", run: func(i int, text string) []asr.Detection {
			explicit := asr.DetectReferences(text)
			matches, err := repo.MatchQuote(ctx, translationID, text, quoteMatchLimit)
			if err != nil {
				return nil
			}
			return detection.DetectQuotes(text, matches, explicit, nil)
		}})
	}
	return lanes
}

// setPath is the directory of the active benchmark set; one process examines
// one set at a time.
var setPath string

func readManifest(path string) (manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	// Windows tools (PowerShell < 7) emit UTF-8 with BOM; strip it.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

func transcribeClips(engine asr.Engine, set manifest, workDir, dump string) ([]string, []error, int64) {
	texts := make([]string, len(set.Clips))
	errs := make([]error, len(set.Clips))
	var elapsed int64

	for i, clip := range set.Clips {
		if clip.File == "" {
			// Detector-only clip passing through an engine run.
			texts[i], errs[i] = clipTranscript(clip)
			continue
		}

		audioPath := filepath.Join(setPath, clip.File)
		ready, err := asr.Prepare16kMono(audioPath, workDir)
		if err != nil {
			errs[i] = err
			continue
		}

		start := time.Now()
		text, err := engine.Transcribe(ready)
		elapsed += int64(time.Since(start) / time.Millisecond)
		if err != nil {
			errs[i] = err
			continue
		}
		texts[i] = text
		if dump != "" {
			writeDump(dump, engine.Name(), clipName(clip, i), text)
		}
	}
	return texts, errs, elapsed
}

// detectorOnlyTranscripts prefers gold manifest text, then a sibling .txt file.
func detectorOnlyTranscripts(set manifest) ([]string, []error) {
	texts := make([]string, len(set.Clips))
	errs := make([]error, len(set.Clips))
	for i, clip := range set.Clips {
		texts[i], errs[i] = clipTranscript(clip)
	}
	return texts, errs
}

func clipTranscript(clip benchClip) (string, error) {
	if clip.Transcript != "" {
		return clip.Transcript, nil
	}
	if clip.File == "" {
		return "", fmt.Errorf("clip has neither file nor gold transcript")
	}
	base := strings.TrimSuffix(clip.File, filepath.Ext(clip.File))
	txt := filepath.Join(setPath, base+".txt")
	if raw, err := os.ReadFile(txt); err == nil {
		return string(raw), nil
	}
	return "", fmt.Errorf("no transcript source for %q", clip.File)
}

func clipName(clip benchClip, index int) string {
	if clip.ID != "" {
		return clip.ID
	}
	if clip.File != "" {
		return strings.TrimSuffix(clip.File, filepath.Ext(clip.File))
	}
	return fmt.Sprintf("clip-%02d", index)
}

type goldRefs = [][]scripture.Reference

func parseGold(clips []benchClip) goldRefs {
	gold := make(goldRefs, len(clips))
	for i, clip := range clips {
		for _, ref := range clip.References {
			parsed, err := scripture.Parse(ref)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: manifest gold %q: %v\n", ref, err)
				continue
			}
			gold[i] = append(gold[i], parsed)
		}
	}
	return gold
}

func parseEstablished(clips []benchClip) []scripture.Reference {
	out := make([]scripture.Reference, len(clips))
	for i, clip := range clips {
		if clip.Established == "" {
			continue
		}
		ref, err := scripture.Parse(clip.Established)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: clip %s established %q: %v\n", clipName(clip, i), clip.Established, err)
			continue
		}
		out[i] = ref
	}
	return out
}

// benchmarkRun is one engine's output over the manifest.
type benchmarkRun struct {
	name    string
	texts   []string
	errs    []error
	elapsed int64
}

func scoreRun(set manifest, gold goldRefs, run benchmarkRun, lanes []laneDef) engineSummary {
	summary := engineSummary{Engine: run.name, TranscribeMs: run.elapsed}

	for _, def := range lanes {
		lane := laneSummary{Name: def.name}
		for i := range set.Clips {
			clip := set.Clips[i]
			if run.errs[i] != nil {
				result := clipResult{Clip: clipName(clip, i), Expected: clip.References,
					Detected: []string{fmt.Sprintf("ERROR: %v", run.errs[i])}, Missed: clip.References}
				lane.Clips = append(lane.Clips, result)
				lane.Expected += len(clip.References)
				continue
			}
			accumulate(&lane, clip, i, run.texts[i], def.run(i, run.texts[i]), gold[i])
		}
		lane.Calibration = confidence.Sweep(lane.observations, lane.Expected, confidence.DefaultThresholds())
		summary.Lanes = append(summary.Lanes, lane)
	}

	// WER is meaningful only when the transcript came out of an engine;
	// detector-only mode echoes the gold text, so scoring it would be 0.
	if run.name != "detector-only" {
		for i := range set.Clips {
			clip := set.Clips[i]
			if run.errs[i] != nil || clip.Transcript == "" {
				continue
			}
			summary.MeanWER += wordErrorRate(run.texts[i], clip.Transcript)
			summary.WERClips++
		}
		if summary.WERClips > 0 {
			summary.MeanWER /= float64(summary.WERClips)
		}
	}
	return summary
}

// accumulate matches one clip's detections against its gold on book+chapter
// (verse precision is reported separately because ranges legitimately widen)
// and records the confidence each detection would carry in the app.
func accumulate(lane *laneSummary, clip benchClip, index int, transcript string, detections []asr.Detection, gold []scripture.Reference) {
	result := clipResult{Clip: clipName(clip, index), Expected: clip.References, Transcript: transcript}

	matchedDetection := make([]bool, len(detections))
	matchedGold := make([]bool, len(gold))
	for d := range detections {
		for g := range gold {
			if matchedGold[g] {
				continue
			}
			if detections[d].Reference.Book.ID == gold[g].Book.ID &&
				detections[d].Reference.Chapter == gold[g].Chapter {
				matchedDetection[d] = true
				matchedGold[g] = true
				result.Hits = append(result.Hits, detections[d].Reference.String())
				if detections[d].Reference.VerseStart == gold[g].VerseStart {
					result.VerseExact++
				}
				break
			}
		}
	}
	for d := range detections {
		result.Detected = append(result.Detected, detections[d].Reference.String())
		if !matchedDetection[d] {
			result.Extra = append(result.Extra, detections[d].Reference.String())
		}
	}
	for g := range gold {
		if !matchedGold[g] {
			result.Missed = append(result.Missed, gold[g].String())
		}
	}

	for d := range detections {
		// A quote or contextual detection already carries the confidence the
		// app would show; an explicit reference is scored from its phrasing.
		score := detections[d].Confidence
		if score <= 0 {
			score = confidence.Evaluate(transcript, detections[d].Reference)
		}
		result.Scores = append(result.Scores, score)
		lane.observations = append(lane.observations,
			confidence.Observation{Score: score, Matched: matchedDetection[d]})
		switch confidence.BandOf(score) {
		case confidence.BandHigh:
			if matchedDetection[d] {
				lane.HighCorrect++
			} else {
				lane.HighFalse++
			}
		case confidence.BandMedium:
			if matchedDetection[d] {
				lane.MediumCorrect++
			} else {
				lane.MediumFalse++
			}
		default:
			if matchedDetection[d] {
				lane.LowCorrect++
			} else {
				lane.LowFalse++
			}
		}
	}

	lane.Expected += len(gold)
	lane.Hits += len(result.Hits)
	lane.Extra += len(result.Extra)
	lane.VerseExact += result.VerseExact
	lane.Clips = append(lane.Clips, result)
}

func wordErrorRate(hypothesis, reference string) float64 {
	h := words(hypothesis)
	g := words(reference)
	if len(g) == 0 {
		return 0
	}
	if len(h) == 0 {
		return 1
	}
	prev := make([]int, len(h)+1)
	row := make([]int, len(h)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(g); i++ {
		prev, row = row, prev
		row[0] = i
		for j := 1; j <= len(h); j++ {
			substitution := prev[j-1]
			if g[i-1] != h[j-1] {
				substitution++
			}
			row[j] = minInt(prev[j]+1, row[j-1]+1, substitution)
		}
	}
	return float64(row[len(h)]) / float64(len(g))
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !isWordRune(r)
	})
}

func isWordRune(r rune) bool {
	isAlnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	return isAlnum
}

func minInt(values ...int) int {
	smallest := values[0]
	for _, v := range values[1:] {
		if v < smallest {
			smallest = v
		}
	}
	return smallest
}

func printReport(summaries []engineSummary) {
	for _, s := range summaries {
		fmt.Printf("== %s ==\n", s.Engine)
		for _, lane := range s.Lanes {
			fmt.Printf("  [%s]\n", lane.Name)
			recall := 0.0
			if lane.Expected > 0 {
				recall = float64(lane.Hits) / float64(lane.Expected) * 100
			}
			precision := 0.0
			if lane.Hits+lane.Extra > 0 {
				precision = float64(lane.Hits) / float64(lane.Hits+lane.Extra) * 100
			}
			fmt.Printf("   recall %d/%d (%.1f%%)  precision %.1f%%  verse-exact %d/%d\n",
				lane.Hits, lane.Expected, recall, precision, lane.VerseExact, lane.Expected)
			printConfidence(lane)
			for _, clip := range lane.Clips {
				state := "OK"
				switch {
				case strings.Contains(strings.Join(clip.Detected, ""), "ERROR:"):
					state = "ERROR"
				case len(clip.Missed) > 0:
					state = "MISS"
				case len(clip.Extra) > 0:
					state = "EXTRA"
				}
				fmt.Printf("   %-24s %s", clip.Clip, state)
				if len(clip.Extra) > 0 {
					fmt.Printf("  extra=%v", clip.Extra)
				}
				fmt.Println()
				if len(clip.Missed) > 0 {
					fmt.Printf("        missed %v\n", clip.Missed)
				}
				if state != "OK" && strings.TrimSpace(clip.Transcript) != "" {
					shown := clip.Transcript
					if len(shown) > 140 {
						shown = shown[:140] + " [...]"
					}
					fmt.Printf("        | %s\n", shown)
				}
			}
		}
		if s.MeanWER > 0 {
			fmt.Printf("   mean WER %.1f%% over %d clips\n", s.MeanWER*100, s.WERClips)
		}
		fmt.Println()
	}
}

// printConfidence shows where the bands were right and what moving the
// threshold would have cost, which is the whole point of the labeled set.
func printConfidence(lane laneSummary) {
	detected := lane.HighCorrect + lane.HighFalse +
		lane.MediumCorrect + lane.MediumFalse + lane.LowCorrect + lane.LowFalse
	if detected == 0 {
		return
	}
	fmt.Printf("   confidence  high %d/%d correct  medium %d/%d  low %d/%d\n",
		lane.HighCorrect, lane.HighCorrect+lane.HighFalse,
		lane.MediumCorrect, lane.MediumCorrect+lane.MediumFalse,
		lane.LowCorrect, lane.LowCorrect+lane.LowFalse)
	if len(lane.Calibration) == 0 {
		return
	}
	fmt.Printf("   threshold  kept  correct  false  precision  recall\n")
	for _, point := range lane.Calibration {
		fmt.Printf("     %.2f      %3d   %6d  %5d   %6.1f%%   %5.1f%%\n",
			point.Threshold, point.Kept, point.Correct, point.False,
			point.Precision*100, point.Recall*100)
	}

	// The table is for reading the trade-off; this line is the answer the
	// operator came for, on the PRD's own precision bar.
	best, meetsTarget := confidence.Recommend(lane.Calibration)
	if best.Kept == 0 {
		fmt.Printf("   recommended  none - no detection was kept at any threshold\n")
		return
	}
	if meetsTarget {
		fmt.Printf("   recommended  %.2f (keeps %d, precision %.1f%%, recall %.1f%%) - lowest threshold meeting the %.0f%% projection bar\n",
			best.Threshold, best.Kept, best.Precision*100, best.Recall*100, confidence.TargetPrecision*100)
		return
	}
	fmt.Printf("   recommended  %.2f (keeps %d, precision %.1f%%, recall %.1f%%) - best available; %.0f%% projection bar NOT met\n",
		best.Threshold, best.Kept, best.Precision*100, best.Recall*100, confidence.TargetPrecision*100)
}

func writeJSON(path string, report engineSummaries) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeDump(dir, engine, clip, text string) {
	if err := os.MkdirAll(filepath.Join(dir, engine), 0o755); err != nil {
		return
	}
	safe := strings.NewReplacer(
		":", "", "/", "-", "\\", "-", "?", "-", "*", "-", "\"", "'",
	).Replace(clip)
	_ = os.WriteFile(filepath.Join(dir, engine, safe+".txt"), []byte(text), 0o644)
}

func scratchDir() (string, func()) {
	dir, err := os.MkdirTemp("", "asr-bench")
	if err != nil {
		exit(err.Error())
	}
	return dir, func() { _ = os.RemoveAll(dir) }
}

func exit(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

type multiFlag struct{ values []string }

func (m *multiFlag) String() string { return strings.Join(m.values, "\n") }

func (m *multiFlag) Set(value string) error {
	m.values = append(m.values, value)
	return nil
}
