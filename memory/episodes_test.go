package memory

import (
	"testing"
	"time"
)

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want float64
	}{
		{"empty sets", nil, nil, 0},
		{"one empty", []string{"a"}, nil, 0},
		{"full overlap", []string{"a", "b"}, []string{"A", "B"}, 1.0},
		{"partial overlap", []string{"a", "b", "c"}, []string{"b", "c", "d"}, 0.5},
		{"no overlap", []string{"a"}, []string{"b"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jaccardSimilarity(tt.a, tt.b)
			if diff := got - tt.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("jaccardSimilarity(%v, %v) = %f, want %f", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCompositeDistance(t *testing.T) {
	// Two memories with identical embeddings but no entity overlap.
	emb := make([]float32, 10)
	for i := range emb {
		emb[i] = 1.0
	}
	a := EpisodeMemory{Embedding: emb, Entities: []string{"Go", "Rust"}}
	b := EpisodeMemory{Embedding: emb, Entities: []string{"Python", "Java"}}
	c := EpisodeMemory{Embedding: emb, Entities: []string{"Go", "Rust"}}

	distAB := compositeDistance(a, b)
	distAC := compositeDistance(a, c)

	// Same embedding means cosineDist ≈ 0. Entity overlap should pull AC down.
	if distAC >= distAB {
		t.Errorf("entity overlap should reduce distance: distAC=%f >= distAB=%f", distAC, distAB)
	}
	if distAC >= 0 {
		t.Errorf("full entity overlap with identical embedding should be negative: %f", distAC)
	}
}

func TestClusterSpansDays(t *testing.T) {
	day1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)

	sameDay := []EpisodeMemory{
		{CreatedAt: day1},
		{CreatedAt: day1.Add(2 * time.Hour)},
	}
	multiDay := []EpisodeMemory{
		{CreatedAt: day1},
		{CreatedAt: day2},
	}

	if clusterSpansDays(sameDay) {
		t.Error("same-day cluster should not span days")
	}
	if !clusterSpansDays(multiDay) {
		t.Error("multi-day cluster should span days")
	}
	if clusterSpansDays(nil) {
		t.Error("empty cluster should not span days")
	}
}
