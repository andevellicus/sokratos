package memory

import (
	"math"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "basic lowercase and split",
			input: "Hello World",
			want:  []string{"hello", "world"},
		},
		{
			name:  "strips punctuation",
			input: "son's name, family!",
			want:  []string{"son", "s", "name", "family"},
		},
		{
			name:  "removes stop words",
			input: "What is my son's name",
			want:  []string{"son", "s", "name"},
		},
		{
			name:  "preserves digits",
			input: "meeting at 3pm on 2024-01-15",
			want:  []string{"meeting", "3pm", "2024", "01", "15"},
		},
		{
			name:  "empty input",
			input: "",
			want:  []string{},
		},
		{
			name:  "only stop words",
			input: "the is a an and or in of to for",
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Tokenize(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("Tokenize(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("Tokenize(%q)[%d] = %q, want %q", tt.input, i, g, tt.want[i])
				}
			}
		})
	}
}

func TestComputeBM25(t *testing.T) {
	// Hand-calculated example:
	// Corpus: N=100, avgdl=10
	// Query: "son name"
	// Document: "son name is alexander" → tokens after stop words: ["son","name","alexander"]
	// df("son")=5, df("name")=20
	//
	// IDF("son")  = ln((100 - 5 + 0.5)/(5 + 0.5) + 1)   = ln(95.5/5.5 + 1) = ln(18.3636) ≈ 2.9095
	// IDF("name") = ln((100 - 20 + 0.5)/(20 + 0.5) + 1)  = ln(80.5/20.5 + 1) = ln(4.9268) ≈ 1.5948
	//
	// docLen=3, avgdl=10
	// For "son": tf=1
	//   num = 1 * (1.2 + 1) = 2.2
	//   denom = 1 + 1.2*(1 - 0.75 + 0.75*3/10) = 1 + 1.2*(0.25 + 0.225) = 1 + 0.57 = 1.57
	//   contribution = 2.9095 * 2.2 / 1.57 ≈ 4.0756
	//
	// For "name": tf=1
	//   same denom = 1.57
	//   contribution = 1.5948 * 2.2 / 1.57 ≈ 2.2337
	//
	// Total ≈ 6.3093

	stats := CorpusStats{
		TotalDocs: 100,
		AvgDocLen: 10,
		DocFreqs:  map[string]int{"son": 5, "name": 20},
	}
	docTokens := Tokenize("son name alexander")
	queryTokens := Tokenize("son name")

	score := ComputeBM25(docTokens, queryTokens, stats)

	// Allow small floating-point tolerance.
	expected := 6.3093
	if math.Abs(score-expected) > 0.05 {
		t.Errorf("ComputeBM25 = %.4f, want ≈ %.4f", score, expected)
	}
}

func TestComputeBM25_TermNotInCorpus(t *testing.T) {
	// A query term with df=0 should still produce a valid (positive) IDF,
	// not cause division by zero.
	stats := CorpusStats{
		TotalDocs: 100,
		AvgDocLen: 10,
		DocFreqs:  map[string]int{}, // no terms in corpus stats
	}
	docTokens := []string{"rare", "word"}
	queryTokens := []string{"rare"}

	score := ComputeBM25(docTokens, queryTokens, stats)
	if score <= 0 {
		t.Errorf("ComputeBM25 with unknown term should be positive, got %.4f", score)
	}
}

func TestComputeBM25_EmptyInputs(t *testing.T) {
	stats := CorpusStats{TotalDocs: 10, AvgDocLen: 5, DocFreqs: map[string]int{}}

	if s := ComputeBM25(nil, []string{"hello"}, stats); s != 0 {
		t.Errorf("empty doc should score 0, got %.4f", s)
	}
	if s := ComputeBM25([]string{"hello"}, nil, stats); s != 0 {
		t.Errorf("empty query should score 0, got %.4f", s)
	}
	if s := ComputeBM25(nil, nil, stats); s != 0 {
		t.Errorf("both empty should score 0, got %.4f", s)
	}
}

func TestIsDuplicate(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		threshold float64
		want      bool
	}{
		{
			name:      "identical texts",
			a:         "son name is alexander",
			b:         "son name is alexander",
			threshold: 0.8,
			want:      true,
		},
		{
			name:      "near duplicate with minor addition",
			a:         "User's son is named Alexander",
			b:         "User's son is named Alexander. He is 5 years old.",
			threshold: 0.8,
			want:      true,
		},
		{
			name:      "completely different",
			a:         "doctor appointment tomorrow morning",
			b:         "stock market performance quarterly report",
			threshold: 0.8,
			want:      false,
		},
		{
			name:      "empty string",
			a:         "",
			b:         "hello world",
			threshold: 0.8,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDuplicate(tt.a, tt.b, tt.threshold)
			if got != tt.want {
				t.Errorf("IsDuplicate(%q, %q, %.1f) = %v, want %v",
					tt.a, tt.b, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestMinMaxNormalize(t *testing.T) {
	t.Run("normal range", func(t *testing.T) {
		got := MinMaxNormalize([]float64{1, 5, 3})
		want := []float64{0, 1, 0.5}
		for i, g := range got {
			if math.Abs(g-want[i]) > 1e-9 {
				t.Errorf("MinMaxNormalize[%d] = %.4f, want %.4f", i, g, want[i])
			}
		}
	})

	t.Run("all equal", func(t *testing.T) {
		got := MinMaxNormalize([]float64{3, 3, 3})
		for i, g := range got {
			if g != 0.5 {
				t.Errorf("MinMaxNormalize (all equal)[%d] = %.4f, want 0.5", i, g)
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		got := MinMaxNormalize(nil)
		if got != nil {
			t.Errorf("MinMaxNormalize(nil) = %v, want nil", got)
		}
	})

	t.Run("single element", func(t *testing.T) {
		got := MinMaxNormalize([]float64{7})
		if got[0] != 0.5 {
			t.Errorf("MinMaxNormalize single = %.4f, want 0.5", got[0])
		}
	})
}
