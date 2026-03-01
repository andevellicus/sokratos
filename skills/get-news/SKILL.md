---
name: get-news
description: |
  Search for recent news articles via SearXNG filtered by configured sources and topics.
  Reads default sources and topics from config.txt. Args override config when provided.
  Results include title, URL, and snippet — always include the URL when presenting results.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| sources | string | no |
| topics | string | no |
| count | number | no |
