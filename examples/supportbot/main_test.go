package main

import "testing"

func TestComputeStats(t *testing.T) {
	yes, no := true, false
	evs := []event{
		{Type: "chat", ID: "a", Session: "s1", Cache: "miss", RttMs: 1000},
		{Type: "chat", ID: "b", Session: "s2", Cache: "hit", RttMs: 1},
		{Type: "chat", ID: "c", Session: "s2", Cache: "hit", RttMs: 3},
		{Type: "chat", ID: "d", Session: "s3", Cache: "bypass"},
		{Type: "chat", ID: "e", Cache: "error"},
		{Type: "feedback", ID: "b", Helpful: &yes},
		{Type: "feedback", ID: "c", Helpful: &no},
		{Type: "feedback", ID: "a", Helpful: &yes},
	}
	s := computeStats(evs)
	if s.Questions != 4 || s.Hits != 2 || s.Misses != 1 || s.Bypassed != 1 || s.Errors != 1 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.HitRate != 50 || s.Students != 3 {
		t.Fatalf("hit rate or students wrong: %+v", s)
	}
	if s.DownCached != 1 || s.HelpfulHit != "1 of 2 (50%)" || s.HelpfulMiss != "1 of 1 (100%)" {
		t.Fatalf("feedback wrong: %+v", s)
	}
}

func TestTypedStatsIgnoreChipsAndRepeats(t *testing.T) {
	evs := []event{
		{Type: "chat", ID: "1", Session: "s1", Question: "How do I pay fees?", Cache: "miss", Source: "typed"},
		{Type: "chat", ID: "2", Session: "s1", Question: "how do i pay fees? ", Cache: "hit", Source: "typed"}, // same student repeats: not counted
		{Type: "chat", ID: "3", Session: "s2", Question: "How do I pay fees?", Cache: "hit", Source: "typed"},
		{Type: "chat", ID: "4", Session: "s3", Question: "Tell me about Library", Cache: "hit", Source: "chip"}, // button click: not counted
		{Type: "chat", ID: "5", Session: "s3", Question: "library timing?", Cache: "miss"},                      // old log line, no source: treated as typed
	}
	s := computeStats(evs)
	if s.TypedQuestions != 3 || s.TypedHits != 1 || s.TypedHitRate != 33.3 {
		t.Fatalf("typed stats wrong: %+v", s)
	}
	if s.Hits != 3 || s.Questions != 5 {
		t.Fatalf("overall stats wrong: %+v", s)
	}
}

func TestTopicsFrom(t *testing.T) {
	faq := "Library: open 9 to 8.\nBonafide certificate: ask on the portal.\nID card: apply at the office.\nA very long sentence that is not a topic name at all, really: x\nLibrary: duplicate"
	got := topicsFrom(faq)
	if len(got) != 3 {
		t.Fatalf("want 3 topics, got %+v", got)
	}
	if got[1].Icon != "file" || got[2].Icon != "id" {
		t.Fatalf("icons wrong: %+v", got)
	}
}

func TestSavings(t *testing.T) {
	evs := []event{
		{Type: "chat", ID: "a", Session: "s", Question: "q1", Cache: "miss", RttMs: 2000, Tokens: 400},
		{Type: "chat", ID: "b", Session: "s", Question: "q1", Cache: "hit", RttMs: 1, Tokens: 400},
		{Type: "chat", ID: "c", Session: "s", Question: "q1", Cache: "hit", RttMs: 1, Tokens: 400},
		{Type: "chat", ID: "d", Session: "s", Question: "otp 123456", Cache: "bypass", RttMs: 1500},
	}
	s := computeStats(evs)
	if s.CallsAvoided != 2 || s.TokensAvoided != 800 {
		t.Fatalf("calls=%d tokens=%d", s.CallsAvoided, s.TokensAvoided)
	}
	if s.WaitSavedSec < 3.9 || s.WaitSavedSec > 4.1 {
		t.Fatalf("wait saved = %v, want ~4", s.WaitSavedSec)
	}
	// No misses measured yet: do not claim time saved.
	if got := computeStats(evs[1:3]).WaitSavedSec; got != 0 {
		t.Fatalf("claimed %v s without a measured model call", got)
	}
}
