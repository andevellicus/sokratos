package memory

import (
	"math"
	"strings"
	"unicode"
)

// CorpusStats holds corpus-level statistics needed for BM25 IDF computation.
type CorpusStats struct {
	TotalDocs int
	AvgDocLen float64
	DocFreqs  map[string]int // term → number of docs containing it
}

// BM25 parameters (standard Okapi BM25 defaults).
const (
	bm25K1 = 1.2  // term frequency saturation
	bm25B  = 0.75 // length normalization
)

// stopWords is a small set of English stop words removed during tokenization.
var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "at": {}, "be": {}, "but": {},
	"by": {}, "for": {}, "from": {}, "in": {}, "is": {}, "it": {}, "my": {},
	"not": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {}, "this": {},
	"to": {}, "was": {}, "what": {}, "with": {},
}

// Tokenize lowercases text, splits on non-alphanumeric characters, and
// removes English stop words. Returns a slice of tokens (may contain
// duplicates — needed for term frequency counting).
func Tokenize(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	tokens := make([]string, 0, len(words))
	for _, w := range words {
		if _, stop := stopWords[w]; !stop && len(w) > 0 {
			tokens = append(tokens, w)
		}
	}
	return tokens
}

// tokenSet returns the unique tokens in text as a set.
func tokenSet(text string) map[string]struct{} {
	tokens := Tokenize(text)
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	return set
}

// ComputeBM25 scores a single document against a query using Okapi BM25.
//
//	IDF(t) = ln((N - df(t) + 0.5) / (df(t) + 0.5) + 1)
//	score  = Σ IDF(t) * (tf(t,d) * (k1+1)) / (tf(t,d) + k1*(1 - b + b*|d|/avgdl))
func ComputeBM25(docTokens []string, queryTokens []string, stats CorpusStats) float64 {
	if len(docTokens) == 0 || len(queryTokens) == 0 {
		return 0
	}

	// Term frequency in document.
	tf := make(map[string]int, len(docTokens))
	for _, t := range docTokens {
		tf[t]++
	}

	docLen := float64(len(docTokens))
	avgDL := stats.AvgDocLen
	if avgDL == 0 {
		avgDL = docLen // avoid division by zero
	}
	n := float64(stats.TotalDocs)

	// Deduplicate query terms so each term contributes once.
	seen := make(map[string]struct{}, len(queryTokens))
	var score float64
	for _, qt := range queryTokens {
		if _, dup := seen[qt]; dup {
			continue
		}
		seen[qt] = struct{}{}

		df := float64(stats.DocFreqs[qt])
		idf := math.Log((n-df+0.5)/(df+0.5) + 1)

		termFreq := float64(tf[qt])
		num := termFreq * (bm25K1 + 1)
		denom := termFreq + bm25K1*(1-bm25B+bm25B*docLen/avgDL)
		score += idf * num / denom
	}

	return score
}

// IsDuplicate returns true if two texts share more than threshold fraction
// of their tokens (measured against the smaller set). This catches chunked
// duplicates and near-identical triaged memories.
func IsDuplicate(a, b string, threshold float64) bool {
	setA := tokenSet(a)
	setB := tokenSet(b)
	if len(setA) == 0 || len(setB) == 0 {
		return false
	}

	overlap := 0
	// Iterate over the smaller set for efficiency.
	small, big := setA, setB
	if len(setA) > len(setB) {
		small, big = setB, setA
	}
	for t := range small {
		if _, ok := big[t]; ok {
			overlap++
		}
	}

	smaller := len(small)
	return float64(overlap)/float64(smaller) > threshold
}

// MinMaxNormalize scales values to [0,1] using min-max normalization.
// Returns a new slice. If all values are equal, returns all 0.5.
func MinMaxNormalize(values []float64) []float64 {
	if len(values) == 0 {
		return nil
	}
	min, max := values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	result := make([]float64, len(values))
	rng := max - min
	if rng == 0 {
		for i := range result {
			result[i] = 0.5
		}
		return result
	}
	for i, v := range values {
		result[i] = (v - min) / rng
	}
	return result
}
