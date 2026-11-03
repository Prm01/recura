// Package embed turns text into unit-length vectors so that prompts with
// the same meaning land close together.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Embedder converts text to a normalised vector. Implementations must be
// safe for concurrent use.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Name() string
}

// Cosine returns the cosine similarity of two vectors. Vectors produced by
// this package are already unit length, so this is a dot product, but it
// normalises anyway so callers can pass arbitrary vectors.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// Normalize scales v to unit length in place and returns it.
func Normalize(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// DefaultStopwords are dropped before hashing. They carry little meaning
// and would otherwise make unrelated questions look similar.
var DefaultStopwords = []string{
	"a", "an", "the", "is", "are", "am", "be", "i", "me", "my", "you", "your",
	"it", "of", "in", "on", "to", "for", "with", "this", "that", "there",
	"how", "do", "does", "what", "when", "can", "will", "want", "please",
	"now", "long", "take",
	// chat filler that carries no meaning
	"hey", "hi", "hello", "pls", "plz", "thanks", "thank", "thx", "whats", "quick", "question", "asap",
}

// Hash is a dependency-free embedder based on feature hashing of words and
// character trigrams. It understands spelling variants and word overlap,
// not deep meaning, so it is ideal for tests, demos and FAQ-style traffic.
// Use an API embedder for open-ended production traffic.
type Hash struct {
	Dim      int
	stop     map[string]bool
	synonyms map[string]string
}

// NewHash builds a hash embedder. synonyms maps a word to its canonical
// form; mapping to "" drops the word (useful for filler words in other
// languages, e.g. Hinglish "kya", "hai").
func NewHash(dim int, stopwords []string, synonyms map[string]string) *Hash {
	if dim <= 0 {
		dim = 512
	}
	h := &Hash{Dim: dim, stop: map[string]bool{}, synonyms: map[string]string{}}
	for _, w := range stopwords {
		h.stop[strings.ToLower(w)] = true
	}
	for k, v := range synonyms {
		h.synonyms[strings.ToLower(k)] = strings.ToLower(v)
	}
	return h
}

func (h *Hash) Name() string { return fmt.Sprintf("hash-%d", h.Dim) }

// Tokens returns the normalised tokens the embedder hashes. Exported for
// debugging and tests.
func (h *Hash) Tokens(text string) []string {
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := f[:0]
	for _, w := range f {
		if s, ok := h.synonyms[w]; ok {
			w = s
		}
		if w == "" || h.stop[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

func (h *Hash) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, h.Dim)
	add := func(feature string, weight float32) {
		f := fnv.New64a()
		_, _ = f.Write([]byte(feature))
		x := f.Sum64()
		idx := int(x % uint64(h.Dim))
		if x&(1<<63) != 0 {
			weight = -weight
		}
		v[idx] += weight
	}
	for _, w := range h.Tokens(text) {
		add("w:"+w, 2)
		p := []rune("#" + w + "#")
		for i := 0; i+3 <= len(p); i++ {
			add("t:"+string(p[i:i+3]), 1)
		}
	}
	return Normalize(v), nil
}

// OpenAI calls any OpenAI-compatible /embeddings endpoint (OpenAI, Azure
// OpenAI, Ollama, LM Studio, vLLM, Together and others).
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string // e.g. text-embedding-3-small
	Client  *http.Client
}

func (o *OpenAI) Name() string { return "openai:" + o.Model }

func (o *OpenAI) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.Model, "input": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	c := o.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embeddings: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embeddings: decode: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errors.New("embeddings: empty response")
	}
	return Normalize(out.Data[0].Embedding), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Dot is the dot product of two equal-length vectors. For unit vectors it
// equals cosine similarity and is several times cheaper than Cosine.
func Dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}
