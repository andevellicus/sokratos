package memory

import (
	"strings"
	"testing"
)

func TestRankingOrderBy(t *testing.T) {
	sql := RankingOrderBy(1, 2)

	// Verify parameter placeholders.
	if !strings.Contains(sql, "$1") {
		t.Error("RankingOrderBy(1,2) missing $1 placeholder")
	}
	if !strings.Contains(sql, "$2") {
		t.Error("RankingOrderBy(1,2) missing $2 placeholder")
	}

	// Verify key SQL fragments.
	fragments := []string{
		"embedding <=>",
		"ts_rank",
		"salience",
		"entities",
		"usefulness_score",
		"retrieval_count",
		"confidence",
	}
	for _, f := range fragments {
		if !strings.Contains(sql, f) {
			t.Errorf("RankingOrderBy() missing fragment %q", f)
		}
	}

	// Different params produce different output.
	sql2 := RankingOrderBy(3, 4)
	if !strings.Contains(sql2, "$3") || !strings.Contains(sql2, "$4") {
		t.Error("RankingOrderBy(3,4) should use $3 and $4 placeholders")
	}
}
