package asr

import "testing"

func TestHasCorrectionCue(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"no wait, it's actually Romans 8:29", true},
		{"i mean second Corinthians 5:17", true},
		{"let me correct that, it's verse twenty nine", true},
		{"scratch that, turn to John 3:16", true},
		{"never mind, the passage is Isaiah 41:10", true},
		{"turn to John 3:16 this morning", false},
		{"and he said, believe and be saved", false},
	}
	for _, c := range cases {
		if got := HasCorrectionCue(c.text); got != c.want {
			t.Errorf("HasCorrectionCue(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
