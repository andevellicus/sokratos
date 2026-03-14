package toolreg

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"sokratos/logger"
	"sokratos/memory"
)

var coreTools = map[string]struct{}{
	"search_memory":        {},
	"save_memory":          {},
	"set_preference":       {},
	"update_state":         {},
	"search_web":           {},
	"read_url":             {},
	"deep_think":           {},
	"consult_deep_thinker": {},
}

var affinityGroups = [][]string{
	{"search_email", "send_email"},
	{"search_calendar", "create_event"},
	{"read_file", "write_file", "patch_file", "list_files"},
	{"search_memory", "save_memory", "consolidate_memory"},
	{"create_skill", "manage_skills", "update_skill"},
	{"delegate_task", "plan_and_execute", "manage_tasks"},
	{"manage_routines"},
	{"manage_objectives"},
}

type toolEmbedding struct {
	Name      string
	Embedding []float32
}

// ToolSelector selects a relevant subset of tools for each request using
// embedding-based similarity.
type ToolSelector struct {
	mu            sync.RWMutex
	embeddings    []toolEmbedding
	embedEndpoint string
	embedModel    string
	k             int
}

// NewToolSelector creates a ToolSelector.
func NewToolSelector(embedEndpoint, embedModel string, k int) *ToolSelector {
	return &ToolSelector{
		embedEndpoint: embedEndpoint,
		embedModel:    embedModel,
		k:             k,
	}
}

// UpdateEmbeddings computes and caches embedding vectors for all tool descriptions.
func (ts *ToolSelector) UpdateEmbeddings(ctx context.Context, registry *Registry) error {
	schemas := registry.Schemas()

	var names []string
	var texts []string
	for _, s := range schemas {
		if s.Name == "respond" {
			continue
		}
		desc := s.Description
		if desc == "" {
			desc = s.Name
		}
		names = append(names, s.Name)
		texts = append(texts, s.Name+": "+desc)
	}

	if len(texts) == 0 {
		return nil
	}

	embeddings, err := memory.GetEmbeddings(ctx, ts.embedEndpoint, ts.embedModel, texts)
	if err != nil {
		return fmt.Errorf("embed tool descriptions: %w", err)
	}

	entries := make([]toolEmbedding, len(names))
	for i := range names {
		entries[i] = toolEmbedding{Name: names[i], Embedding: embeddings[i]}
	}

	ts.mu.Lock()
	ts.embeddings = entries
	ts.mu.Unlock()

	logger.Log.Infof("[tool-selector] embedded %d tool descriptions", len(entries))
	return nil
}

type scoredTool struct {
	Name  string
	Score float64
}

// Select returns the subset of tools relevant to the given query.
func (ts *ToolSelector) Select(ctx context.Context, query string) ([]string, error) {
	ts.mu.RLock()
	embeddings := ts.embeddings
	ts.mu.RUnlock()

	if len(embeddings) == 0 {
		return nil, nil
	}

	queryEmb, err := memory.GetEmbedding(ctx, ts.embedEndpoint, ts.embedModel, query)
	if err != nil {
		return nil, fmt.Errorf("embed query for tool selection: %w", err)
	}

	scored := make([]scoredTool, len(embeddings))
	for i, te := range embeddings {
		scored[i] = scoredTool{Name: te.Name, Score: memory.CosineSimilarity(queryEmb, te.Embedding)}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	topK := ts.k
	if topK > len(scored) {
		topK = len(scored)
	}
	selected := make(map[string]struct{})
	for i := 0; i < topK; i++ {
		selected[scored[i].Name] = struct{}{}
	}

	for _, group := range affinityGroups {
		hit := false
		for _, t := range group {
			if _, ok := selected[t]; ok {
				hit = true
				break
			}
		}
		if hit {
			for _, t := range group {
				selected[t] = struct{}{}
			}
		}
	}

	for t := range coreTools {
		selected[t] = struct{}{}
	}

	names := make([]string, 0, len(selected))
	for t := range selected {
		names = append(names, t)
	}
	sort.Strings(names)

	logger.Log.Debugf("[tool-selector] selected %d tools for query: %s", len(names), truncateQuery(query, 80))
	return names, nil
}

// BuildSelectedToolIndex builds tool descriptions for a subset of tools.
func BuildSelectedToolIndex(registry *Registry, names []string) string {
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}

	var builtins strings.Builder
	var skills strings.Builder
	hasSkills := false

	for _, s := range registry.Schemas() {
		if _, ok := nameSet[s.Name]; !ok {
			continue
		}
		if s.Name == "respond" {
			continue
		}

		if s.IsSkill {
			hasSkills = true
			skills.WriteString("- ")
			skills.WriteString(s.Name)
			skills.WriteString(": ")
			if s.Description != "" {
				skills.WriteString(s.Description)
			}
			if len(s.Params) > 0 {
				skills.WriteString(" Arguments: {")
				for i, p := range s.Params {
					if i > 0 {
						skills.WriteString(", ")
					}
					fmt.Fprintf(&skills, "\"%s\": \"<%s>\"", p.Name, p.Type)
				}
				skills.WriteString("}")
			} else {
				skills.WriteString(" No arguments.")
			}
			skills.WriteString("\n")
		} else {
			builtins.WriteString("- ")
			builtins.WriteString(s.Name)
			if len(s.Params) > 0 {
				builtins.WriteString("(")
				for i, p := range s.Params {
					if i > 0 {
						builtins.WriteString(", ")
					}
					if p.Required {
						builtins.WriteString("*")
					}
					builtins.WriteString(p.Name)
				}
				builtins.WriteString(")")
			}
			if s.Description != "" {
				builtins.WriteString(" — ")
				builtins.WriteString(s.Description)
			}
			builtins.WriteString("\n")
		}
	}

	idx := builtins.String()
	if hasSkills {
		idx += "\n## Skills\n\n" + skills.String()
	}
	return idx
}

func truncateQuery(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
