package asr

import "strings"

// correctionCues are the phrases a speaker uses to take back a reference and
// name the one they meant ("no wait, it's Romans 8:29"). None is a word that
// appears in scripture or a canonical book name, so scanning for one cannot
// confuse a quotation for a correction. The guard that matters lives in the
// queue: a correction only ever replaces a recent card of the same service, so
// a stray "actually" in passing speech changes nothing when there is no card to
// replace.
var correctionCues = []string{
	"no wait", "i mean", "i meant", "actually", "correct that",
	"correction", "scratch that", "never mind", "let me correct",
	"meant to say", "that should be",
}

// HasCorrectionCue reports whether a transcript reads like a speaker correcting
// an earlier reference. The detection layer tags that hearing as a correction,
// which the queue uses to supersede the prior card rather than stack beside it.
func HasCorrectionCue(text string) bool {
	normalized := normalizeSpokenText(text)
	for _, cue := range correctionCues {
		if strings.Contains(normalized, cue) {
			return true
		}
	}
	return false
}
