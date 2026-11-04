// Package privacy decides whether a prompt is too personal to cache.
// Answers to personal prompts are user-specific, and serving them to
// someone else would leak data.
package privacy

import (
	"fmt"
	"regexp"
)

// DefaultPatterns catch common personal-data shapes. They err towards
// bypassing the cache: a false positive costs one model call, a false
// negative can leak data.
var DefaultPatterns = []string{
	`(?i)\b(my|mera|meri|mere)\s+(order|account|balance|invoice|bill|payment|address|phone|ticket|booking|subscription id)\b`,
	`(?i)\border\s*(no\.?|number|id|#)?\s*#?\d{3,}`,
	`#\d{3,}`,
	`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, // email
	`(?:\+?91[\s\-]?)?[6-9]\d{9}\b`,                    // Indian mobile
	`\b(?:\d[ \-]?){13,19}\b`,                          // card-like digit runs
	`\b[A-Z]{5}\d{4}[A-Z]\b`,                           // PAN
	`\b\d{4}\s?\d{4}\s?\d{4}\b`,                        // Aadhaar-like
	`(?i)\b(password|otp|pin|cvv)\s*(is|:)\s*\S+`,      // secrets typed into a prompt
	`\b[A-Za-z0-9._\-]{2,}@[A-Za-z]{2,}\b`,             // UPI ID
}

// Detector matches prompts against patterns.
type Detector struct{ res []*regexp.Regexp }

// New compiles patterns. Pass nil to use DefaultPatterns.
func New(patterns []string) (*Detector, error) {
	if patterns == nil {
		patterns = DefaultPatterns
	}
	d := &Detector{}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("privacy pattern %q: %w", p, err)
		}
		d.res = append(d.res, re)
	}
	return d, nil
}

// Personal reports whether text looks like it contains personal data.
func (d *Detector) Personal(text string) bool {
	for _, re := range d.res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
