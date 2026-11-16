package detection

import (
	"testing"
	"time"

	"zopresent/internal/asr"
)

func correctionDetection(t *testing.T, ref string) asr.Detection {
	t.Helper()
	d := detectionOf(t, ref)
	d.Method = MethodReferenceCorrection
	d.Confidence = 0.9
	return d
}

func TestCorrectionSupersedesRecentCardInSameBook(t *testing.T) {
	queue, clock := newClockedQueue(t)

	queue.BeginSegment()
	if _, err := queue.Finalize("Romans 8:28", []asr.Detection{detectionOf(t, "Romans 8:28")}, "service-1"); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	*clock = clock.Add(20 * time.Second)

	queue.BeginSegment()
	settled, err := queue.Finalize("no wait, I mean Romans 8:29", []asr.Detection{correctionDetection(t, "Romans 8:29")}, "service-1")
	if err != nil {
		t.Fatalf("correction finalize: %v", err)
	}
	if settled.Repeats != 0 {
		t.Errorf("repeats = %d, want a correction, not a repeat", settled.Repeats)
	}

	pending := queue.List("service-1")
	if len(pending) != 1 {
		t.Fatalf("queue holds %d cards, want the correction superseding into one", len(pending))
	}
	card := pending[0]
	if card.Reference.String() != "Romans 8:29" {
		t.Errorf("reference = %q, want Romans 8:29", card.Reference.String())
	}
	if card.Method != MethodReferenceCorrection {
		t.Errorf("method = %q, want %q", card.Method, MethodReferenceCorrection)
	}
	if card.Version < 2 {
		t.Errorf("version = %d, want the correction counted", card.Version)
	}
	if card.Repeats != 0 {
		t.Errorf("repeats = %d, want 0", card.Repeats)
	}
}

func TestCorrectionWithNoPriorCardJustQueues(t *testing.T) {
	queue, _ := newClockedQueue(t)
	queue.BeginSegment()
	settled, err := queue.Finalize("I mean Romans 8:29", []asr.Detection{correctionDetection(t, "Romans 8:29")}, "service-1")
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if settled.Finalized != 1 {
		t.Fatalf("finalized = %d, want 1", settled.Finalized)
	}
	pending := queue.List("service-1")
	if len(pending) != 1 || pending[0].Reference.String() != "Romans 8:29" {
		t.Fatalf("pending = %+v, want the single correction queued", pending)
	}
}

func TestCorrectionOutsideWindowQueuesNewCard(t *testing.T) {
	queue, clock := newClockedQueue(t)
	queue.BeginSegment()
	if _, err := queue.Finalize("Romans 8:28", []asr.Detection{detectionOf(t, "Romans 8:28")}, "service-1"); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	*clock = clock.Add(RepeatWindow + time.Second)
	queue.BeginSegment()
	if _, err := queue.Finalize("no wait Romans 8:29", []asr.Detection{correctionDetection(t, "Romans 8:29")}, "service-1"); err != nil {
		t.Fatalf("correction finalize: %v", err)
	}
	if pending := queue.List("service-1"); len(pending) != 2 {
		t.Fatalf("queue holds %d cards, want a new card outside the window", len(pending))
	}
}

func TestCorrectionDoesNotSupersedeAnotherBook(t *testing.T) {
	queue, clock := newClockedQueue(t)
	queue.BeginSegment()
	if _, err := queue.Finalize("Romans 8:28", []asr.Detection{detectionOf(t, "Romans 8:28")}, "service-1"); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	*clock = clock.Add(20 * time.Second)
	queue.BeginSegment()
	if _, err := queue.Finalize("no wait, it's John 3:16", []asr.Detection{correctionDetection(t, "John 3:16")}, "service-1"); err != nil {
		t.Fatalf("correction finalize: %v", err)
	}
	if pending := queue.List("service-1"); len(pending) != 2 {
		t.Fatalf("queue holds %d cards, want the cross-book 'correction' to stack", len(pending))
	}
}
