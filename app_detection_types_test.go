package main

import (
	"testing"

	"zopresent/internal/detection"
	"zopresent/internal/policy"
)

// The new detection types ship off until the operator calibrates them on
// labeled audio. With nothing enabled, a bare verse mention against an
// established passage queues no contextual card; enabling the type brings it
// back through the same pipeline.
func TestContextualTypeIsOffUntilEnabled(t *testing.T) {
	app := newTestApp(t)
	installTestBible(t, app)
	job29OnTheWall(t, app)

	if _, err := app.settleSegment("now read verse sixteen"); err != nil {
		t.Fatalf("settleSegment: %v", err)
	}
	if found, _ := contextualCandidateOf(t, app); found {
		t.Fatal("contextual card queued while the type is disabled")
	}

	enableDetectionTypes(t, app, detection.MethodContextualReference)
	if _, err := app.settleSegment("now read verse sixteen"); err != nil {
		t.Fatalf("settleSegment after enabling: %v", err)
	}
	found, ref := contextualCandidateOf(t, app)
	if !found || ref != "Job 29:16" {
		t.Fatalf("contextual card = %q (found %v), want Job 29:16 once enabled", ref, found)
	}
}

func TestQuoteTypeIsOffUntilEnabled(t *testing.T) {
	app := newTestApp(t)
	installQuoteBible(t, app)

	result := runSegment(t, app, unnamedPhilippiansQuote)
	for _, candidate := range result.Candidates {
		if candidate.Method == detection.MethodSpokenQuote {
			t.Fatalf("quote card %s queued while the type is disabled", candidate.Reference)
		}
	}

	enableDetectionTypes(t, app, detection.MethodSpokenQuote)
	result = runSegment(t, app, unnamedPhilippiansQuote)
	found := false
	for _, candidate := range result.Candidates {
		if candidate.Method == detection.MethodSpokenQuote {
			found = true
		}
	}
	if !found {
		t.Fatalf("no quote card once enabled; candidates = %+v", result.Candidates)
	}
}

// A correction cue only supersedes when the correction type is enabled. Off,
// "no wait, I mean John 3:18" is an ordinary second reference; on, it replaces
// the earlier John card instead of stacking beside it.
func TestCorrectionTypeIsOffUntilEnabled(t *testing.T) {
	disabled := newTestApp(t)
	installTestBible(t, disabled)
	if _, err := disabled.settleSegment("turn to John 3:16"); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if _, err := disabled.settleSegment("no wait, I mean John 3:18"); err != nil {
		t.Fatalf("correction settle: %v", err)
	}
	if pending := disabled.detection.List(disabled.currentServiceID(disabled.context())); len(pending) != 2 {
		t.Fatalf("queue holds %d cards with correction disabled, want two stacked references", len(pending))
	}

	enabled := newTestApp(t)
	installTestBible(t, enabled)
	enableDetectionTypes(t, enabled, detection.MethodReferenceCorrection)
	if _, err := enabled.settleSegment("turn to John 3:16"); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if _, err := enabled.settleSegment("no wait, I mean John 3:18"); err != nil {
		t.Fatalf("correction settle: %v", err)
	}
	pending := enabled.detection.List(enabled.currentServiceID(enabled.context()))
	if len(pending) != 1 {
		t.Fatalf("queue holds %d cards with correction enabled, want the correction superseding into one", len(pending))
	}
	if pending[0].Method != detection.MethodReferenceCorrection {
		t.Errorf("method = %q, want %q", pending[0].Method, detection.MethodReferenceCorrection)
	}
	if pending[0].Reference.String() != "John 3:18" {
		t.Errorf("reference = %q, want John 3:18", pending[0].Reference.String())
	}
}

func TestExplicitTypeCanBeDisabled(t *testing.T) {
	app := newTestApp(t)
	installTestBible(t, app)

	if err := app.SetDetectionTypeEnabled(detection.MethodExplicitReference, false); err != nil {
		t.Fatalf("disable explicit: %v", err)
	}
	result := runSegment(t, app, "turn to John 3:16")
	if len(result.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none with the explicit type disabled", result.Candidates)
	}
}

// The config view reports the shipped defaults: explicit on, the rest off, so
// the settings window and the panel render the real state on first load.
func TestDetectionConfigReportsTypeDefaults(t *testing.T) {
	app := newTestApp(t)
	config, err := app.GetDetectionConfig()
	if err != nil {
		t.Fatalf("GetDetectionConfig: %v", err)
	}
	if !config.Types.Explicit {
		t.Error("explicit type defaults off, want on")
	}
	if config.Types.Contextual || config.Types.Quote || config.Types.Correction {
		t.Errorf("types = %+v, want contextual/quote/correction off by default", config.Types)
	}

	enableDetectionTypes(t, app, detection.MethodSpokenQuote)
	config, err = app.GetDetectionConfig()
	if err != nil {
		t.Fatalf("GetDetectionConfig after enabling: %v", err)
	}
	if !config.Types.Quote {
		t.Errorf("quote type still off after enabling; types = %+v", config.Types)
	}
}

func TestSetDetectionTypeEnabledRejectsUnknownType(t *testing.T) {
	app := newTestApp(t)
	if err := app.SetDetectionTypeEnabled("not-a-type", true); err == nil {
		t.Fatal("unknown detection type accepted, want an error")
	}
}

// Policy is the only path to the room, so it weighs the type switch itself
// rather than trusting that generation was gated: a card queued by any means
// whose type is off stays in the queue even with every other gate passing.
func TestTypeGateHoldsBackACardWhoseTypeIsOff(t *testing.T) {
	app := autoReadyApp(t)
	candidate := autoCandidate(t, app, "Turn with me to John 3:16", "John 3:16")

	if err := app.SetDetectionTypeEnabled(detection.MethodExplicitReference, false); err != nil {
		t.Fatalf("disable explicit: %v", err)
	}
	if projected := app.autoProject([]detection.Candidate{candidate}); len(projected) != 0 {
		t.Fatalf("projected %v with the type disabled, want nothing", projected)
	}
	verdict := app.autoProjectionVerdict(app.context(), candidate, 1)
	if verdict.Project {
		t.Fatal("verdict projects a card whose type is off")
	}
	foundType := false
	for _, id := range verdict.Failed {
		if id == policy.GateType {
			foundType = true
		}
	}
	if !foundType {
		t.Fatalf("verdict failed %v, want the type gate", verdict.Failed)
	}
}
