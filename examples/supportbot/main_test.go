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
