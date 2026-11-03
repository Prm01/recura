package embed

import (
	"context"
	"testing"
)

func sim(t *testing.T, e Embedder, a, b string) float32 {
	t.Helper()
	va, err := e.Embed(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	vb, _ := e.Embed(context.Background(), b)
	return Cosine(va, vb)
}

func TestHashSimilarity(t *testing.T) {
	h := NewHash(512, DefaultStopwords, nil)
	if s := sim(t, h, "How do I reset my password?", "how do i reset my password"); s < 0.999 {
		t.Fatalf("identical meaning after normalisation should score ~1, got %.3f", s)
	}
	near := sim(t, h, "reset my password", "pls reset my pasword asap")
	far := sim(t, h, "reset my password", "explain kubernetes pods")
	if near <= far {
		t.Fatalf("typo paraphrase (%.3f) should beat unrelated text (%.3f)", near, far)
	}
	if far > 0.3 {
		t.Fatalf("unrelated prompts too similar: %.3f", far)
	}
}

func TestHashSynonyms(t *testing.T) {
	h := NewHash(512, DefaultStopwords, map[string]string{"bhool": "reset", "gaya": "", "kya": "", "karu": "", "forgot": "reset"})
	if s := sim(t, h, "Password bhool gaya, kya karu", "I forgot my password"); s < 0.99 {
		t.Fatalf("synonym mapping should make these equal, got %.3f", s)
	}
}

func TestNormalizeZero(t *testing.T) {
	v := Normalize([]float32{0, 0})
	if v[0] != 0 || Cosine(v, v) != 0 {
		t.Fatal("zero vector should stay zero and score 0")
	}
}
