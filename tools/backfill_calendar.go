package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	cal "google.golang.org/api/calendar/v3"

	"sokratos/calendar"
	"sokratos/logger"
	"sokratos/prompts"
)

// CalendarBackfillConfig holds everything RunCalendarBackfill needs.
type CalendarBackfillConfig struct {
	BackfillBase
	CalendarService *cal.Service
}

// RunCalendarBackfill ingests historical calendar events into the memory store.
func RunCalendarBackfill(cfg CalendarBackfillConfig) {
	days := cfg.BackfillDays
	if days <= 0 {
		days = 90
	}

	now := time.Now()
	timeMin := now.AddDate(0, 0, -days)
	backfillKey := fmt.Sprintf("calendar_%d_%s", days, now.Format("2006-01-02"))
	logger.Log.Infof("[calendar_backfill] starting: last %d days", days)

	// Cache parsed events by ID so BuildItem can look them up after dedup.
	eventCache := make(map[string]*cal.Event)

	RunBackfillPipeline(BackfillPipelineConfig{
		Pool:           cfg.Pool,
		EmbedEndpoint:  cfg.EmbedEndpoint,
		EmbedModel:     cfg.EmbedModel,
		DTC:            cfg.DTC,
		LogPrefix:      "[calendar_backfill]",
		Kind:           "calendar",
		DomainTag:      "calendar",
		TriagePrompt:   strings.TrimSpace(prompts.CalendarTriage),
		ProcessedTable: ProcessedEvents,
		BackfillKey:    backfillKey,
		ThrottleItem:   1 * time.Second,
		ThrottlePage:   3 * time.Second,
		FetchPage: func(ctx context.Context, pageToken string) ([]string, string, error) {
			req := cfg.CalendarService.Events.List("primary").
				TimeMin(timeMin.Format(time.RFC3339)).
				TimeMax(now.Format(time.RFC3339)).
				SingleEvents(true).
				OrderBy("startTime").
				MaxResults(50)
			if pageToken != "" {
				req = req.PageToken(pageToken)
			}
			list, err := req.Do()
			if err != nil {
				return nil, "", err
			}
			ids := make([]string, len(list.Items))
			for i, item := range list.Items {
				ids[i] = item.Id
				eventCache[item.Id] = item
			}
			return ids, list.NextPageToken, nil
		},
		BuildItem: func(ctx context.Context, id string) (*BackfillItem, error) {
			raw, ok := eventCache[id]
			if !ok {
				return nil, fmt.Errorf("event %s not in cache", id)
			}
			delete(eventCache, id) // free memory after use
			e := calendar.ParseEvent(raw)
			var srcDate *time.Time
			if !e.Start.IsZero() {
				d := e.Start
				srcDate = &d
			}
			return &BackfillItem{
				ID:           e.ID,
				DisplayLabel: fmt.Sprintf("%q", e.Summary),
				TriageText:   calendar.FormatEventSummary(e),
				EmbeddingText: func(triageLine string) string {
					return calendar.FormatForEmbedding(e, triageLine)
				},
				SourceDate: srcDate,
			}, nil
		},
	})
}
