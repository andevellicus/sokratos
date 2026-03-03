---
name: get-feeds
description: |
  Fetch recent items from configured feeds (Twitter via RSSHub, Reddit via native API, plus any RSSHub route).
  Reads feed config from config.toml. Args can override count or filter to a specific feed by name.
  Returns JSON: {count, items: [{feed, title, link, summary, source, date}]}.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| count | number | no |
| feed | string | no |

## Feed Types

### `twitter`
Fetches tweets via RSSHub. Supports `lists` (Twitter list IDs) and `accounts` (handles).
Count is distributed evenly across all sources. Requires `TWITTER_AUTH_TOKEN` in `.env`.

### `reddit`
Fetches posts via Reddit's native JSON API. Specify `subreddit` and `sort` (hot, new, top, rising).

### `rsshub`
Generic RSSHub route. Specify `route` (e.g. `/hackernews/best`). See https://docs.rsshub.app for available routes.
