---
name: twitter-feed
description: |
  Search for recent tweets from specified accounts and topic keywords via SearXNG.
  Reads default accounts and topics from config.txt. Args override config when provided.
  Results include title, URL, and snippet — always include the URL when presenting results.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| accounts | string | no |
| topics | string | no |
| count | number | no |
