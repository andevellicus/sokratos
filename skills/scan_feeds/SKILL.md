---
name: scan_feeds
language: typescript
description: |
  Fetch and summarize news from configured RSS, Atom, Reddit, and Twitter sources.
  Fetches headlines, deduplicates, then dispatches parallel subagents to read and
  summarize top articles. DEFAULT tool for all news requests — "what's in the news",
  "check the news", "latest headlines".
  Returns JSON: {count, sources: [{source, category, summary, error}]}.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| count | number | no |
| feed | string | no |
| max_articles | number | no |

The `feed` parameter filters by source name, category (news/tech/social), or fuzzy match.

## Source Types

Configured in `skills/scan_feeds/config.toml` as `[[<category>]]` entries.
Categories: `news`, `tech`, `social` (or any custom category).

### `twitter`
Fetches tweets via RSSHub. Supports `lists` (Twitter list IDs) and `accounts` (handles).
Count is distributed evenly across all sources. Requires `TWITTER_AUTH_TOKEN` in `.env`.

### `reddit`
Fetches posts via Reddit's native RSS/Atom API. Specify `subreddit` and `sort` (hot, new, top, rising).

### `rsshub`
Generic RSSHub route. Specify `route` (e.g. `/hackernews/best`). See https://docs.rsshub.app for available routes.

### `rss`
Direct RSS/Atom feed. Specify `url` pointing to the feed XML.
