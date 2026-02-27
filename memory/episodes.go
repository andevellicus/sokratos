package memory

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/textutil"
)

// --- Episodic Memory Synthesis ---

// EpisodeMemory holds the fields needed for similarity-based clustering.
type EpisodeMemory struct {
	ID        int64
	Summary   string
	Embedding []float32
}

const episodeSynthesisPrompt = `You synthesize related memories into a cohesive episodic narrative. Given a numbered list of related memories, produce a 2-4 sentence summary that:
- Captures the essential thread connecting these memories
- Preserves key facts, names, dates, and outcomes
- Reads as a natural narrative, not a bullet list

Return ONLY the narrative summary. No preamble, no explanation.`

// SynthesizeFunc is the function signature for calling an LLM to synthesize text.
type SynthesizeFunc func(ctx context.Context, systemPrompt, content string) (string, error)

// cosineSimilarity returns the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// clusterBySimilarity groups memories using greedy single-linkage clustering.
// Two memories are linked if their cosine distance < distanceThreshold
// (i.e., similarity > 1 - distanceThreshold).
func clusterBySimilarity(memories []EpisodeMemory, distanceThreshold float64) [][]EpisodeMemory {
	n := len(memories)
	assigned := make([]bool, n)
	var clusters [][]EpisodeMemory

	for i := 0; i < n; i++ {
		if assigned[i] {
			continue
		}
		cluster := []EpisodeMemory{memories[i]}
		assigned[i] = true

		for j := i + 1; j < n; j++ {
			if assigned[j] {
				continue
			}
			// Check if j is similar to any member of the current cluster.
			for _, member := range cluster {
				dist := 1.0 - cosineSimilarity(member.Embedding, memories[j].Embedding)
				if dist < distanceThreshold {
					cluster = append(cluster, memories[j])
					assigned[j] = true
					break
				}
			}
		}

		clusters = append(clusters, cluster)
	}
	return clusters
}

// SynthesizeEpisodes clusters recent memories by semantic similarity and
// synthesizes each cluster into an episodic memory. Returns the number of
// episodes created.
func SynthesizeEpisodes(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel string, synthesize SynthesizeFunc) (int, error) {
	// Query recent non-episode, non-reflection, non-superseded memories from last 24h.
	rows, err := db.Query(ctx,
		`SELECT id, summary, embedding
		 FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type NOT IN ('episode', 'reflection')
		   AND created_at >= now() - INTERVAL '24 hours'
		 ORDER BY created_at DESC
		 LIMIT 50`,
	)
	if err != nil {
		return 0, fmt.Errorf("query recent memories: %w", err)
	}
	defer rows.Close()

	var memories []EpisodeMemory
	for rows.Next() {
		var m EpisodeMemory
		var vec pgvector.Vector
		if err := rows.Scan(&m.ID, &m.Summary, &vec); err != nil {
			continue
		}
		m.Embedding = vec.Slice()
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate recent memories: %w", err)
	}

	if len(memories) < 2 {
		return 0, nil
	}

	// Cluster by cosine similarity (threshold: distance < 0.3 = similarity > 0.7).
	clusters := clusterBySimilarity(memories, 0.3)

	episodeCount := 0
	for _, cluster := range clusters {
		if len(cluster) < 2 {
			continue
		}

		// Build numbered list for synthesis.
		var sb strings.Builder
		constituentIDs := make([]int64, len(cluster))
		for i, m := range cluster {
			constituentIDs[i] = m.ID
			fmt.Fprintf(&sb, "%d. %s\n", i+1, m.Summary)
		}

		// Synthesize episode narrative.
		raw, err := synthesize(ctx, episodeSynthesisPrompt, sb.String())
		if err != nil {
			logger.Log.Warnf("[memory] episode synthesis failed for cluster of %d: %v", len(cluster), err)
			continue
		}
		raw = textutil.StripThinkTags(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}

		// Embed and insert the episode.
		embedded, err := embedWithFallback(ctx, embedEndpoint, embedModel, raw)
		if err != nil {
			logger.Log.Warnf("[memory] episode embedding failed: %v", err)
			continue
		}

		// Insert episode + link constituents in a single transaction.
		tx, txErr := db.Begin(ctx)
		if txErr != nil {
			logger.Log.Warnf("[memory] episode transaction begin failed: %v", txErr)
			continue
		}

		var episodeID int64
		for _, ec := range embedded {
			var id int64
			txErr = tx.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source, related_ids, entities)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), 8.0, []string{"episode"},
				"episode", "episode_synthesis", constituentIDs, []string{},
			).Scan(&id)
			if txErr != nil {
				logger.Log.Warnf("[memory] episode insert failed: %v", txErr)
				break
			}
			if episodeID == 0 {
				episodeID = id
			}
		}
		if txErr != nil || episodeID == 0 {
			tx.Rollback(ctx)
			continue
		}

		// Link constituent memories to the episode.
		for _, cID := range constituentIDs {
			if _, txErr = tx.Exec(ctx,
				`UPDATE memories SET related_ids = array_append(COALESCE(related_ids, ARRAY[]::BIGINT[]), $1) WHERE id = $2`,
				episodeID, cID,
			); txErr != nil {
				logger.Log.Warnf("[memory] episode link constituent id=%d failed: %v", cID, txErr)
				break
			}
		}
		if txErr != nil {
			tx.Rollback(ctx)
			continue
		}

		if txErr = tx.Commit(ctx); txErr != nil {
			logger.Log.Warnf("[memory] episode transaction commit failed: %v", txErr)
			continue
		}

		episodeCount++
		logger.Log.Infof("[memory] synthesized episode id=%d from %d memories", episodeID, len(cluster))
	}

	return episodeCount, nil
}
