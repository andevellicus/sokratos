// scan_feeds: Fetches configured feeds, deduplicates, then dispatches parallel
// subagents to read and summarize top articles per feed group.

declare const args: { count?: number; feed?: string; max_articles?: number };
declare const skill_config: Record<string, any[]> | undefined;
declare function http_request(method: string, url: string, headers: Record<string, string>, body: string): { status: number; body: string; headers: Record<string, string> };
declare function env(key: string): string | undefined;
declare function kv_get(key: string): string | undefined;
declare function kv_set(key: string, value: string): void;
declare function kv_delete(key: string): void;
declare function delegate_batch(tasks: { directive: string; context: string }[]): { result: string; error: string }[];

interface FeedSource {
  name: string;
  type: string;
  category: string;
  count?: number;
  // twitter
  lists?: string[];
  accounts?: string[];
  // reddit
  subreddit?: string;
  sort?: string;
  // rsshub
  route?: string;
  // rss
  url?: string;
}

interface FeedItem {
  feed: string;
  category: string;
  title: string;
  link: string;
  summary: string;
  source: string;
  date: string;
}

interface RsshubItem {
  url?: string;
  id?: string;
  title?: string;
  content_text?: string;
  content_html?: string;
  date_published?: string;
}

interface ParsedItem {
  url: string;
  title: string;
  summary: string;
  date: string;
  score?: number;
}

interface BackoffRecord {
  until: number;
  wait: number;
}

interface FeedResult {
  source: string;
  category: string;
  summary?: string;
  error?: string;
}

const cfg = skill_config || {};
const rsshubBase: string = env("RSSHUB_URL") || "http://localhost:1200";
const FETCH_MULTIPLIER = 4;

// ========================================================================
// Helpers
// ========================================================================

function stripHtml(html: string): string {
  return (html || "").replace(/<[^>]+>/g, "").substring(0, 300);
}

function decodeXmlEntities(s: string): string {
  return (s || "")
    .replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">")
    .replace(/&#39;/g, "'").replace(/&quot;/g, '"');
}

function extractDomain(url: string): string {
  const m = url.match(/^https?:\/\/(?:www\.)?([^\/]+)/);
  return m ? m[1] : "";
}

// --- Dedup helpers (per-feed, persisted via kv_store) ---

function loadSeen(feedName: string): string[] {
  const key = "seen_" + feedName;
  try {
    const raw = kv_get(key);
    if (raw) return JSON.parse(raw);
  } catch (_) {}
  return [];
}

function saveSeen(feedName: string, seenList: string[]): void {
  const key = "seen_" + feedName;
  const trimmed = seenList.length > 200 ? seenList.slice(seenList.length - 200) : seenList;
  kv_set(key, JSON.stringify(trimmed));
}

// --- RSSHub fetch ---

function fetchRsshub(route: string, count: number): RsshubItem[] {
  let url = rsshubBase + route;
  const sep = url.indexOf("?") === -1 ? "?" : "&";
  url += sep + "limit=" + count + "&format=json";
  const resp = http_request("GET", url, {}, "");
  if (resp.status !== 200) {
    console.warn("RSSHub fetch failed for " + route + ": status " + resp.status);
    return [];
  }
  try {
    const data = JSON.parse(resp.body);
    return data.items || [];
  } catch (e) {
    console.warn("RSSHub parse failed for " + route + ": " + e);
    return [];
  }
}

// --- Direct RSS/Atom fetch ---

function fetchDirectRSS(feedUrl: string, count: number): ParsedItem[] {
  const resp = http_request("GET", feedUrl, {
    "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
    "Accept": "application/rss+xml, application/atom+xml, application/xml",
  }, "");
  if (resp.status !== 200) {
    console.warn("Direct RSS fetch failed for " + feedUrl + ": status " + resp.status);
    return [];
  }
  const body = resp.body || "";
  const items: ParsedItem[] = [];

  if (body.indexOf("<entry>") !== -1 || body.indexOf("<entry ") !== -1) {
    // Atom format
    const entries = body.match(/<entry[\s\S]*?<\/entry>/g) || [];
    for (let i = 0; i < entries.length && i < count; i++) {
      const e = entries[i];
      const titleMatch = e.match(/<title[^>]*>([\s\S]*?)<\/title>/);
      const linkMatch = e.match(/<link[^>]*href="([^"]+)"/);
      const updatedMatch = e.match(/<updated>([\s\S]*?)<\/updated>/);
      const summaryMatch = e.match(/<summary[^>]*>([\s\S]*?)<\/summary>/);
      items.push({
        url: linkMatch ? linkMatch[1] : "",
        title: decodeXmlEntities(titleMatch ? titleMatch[1] : ""),
        summary: stripHtml(decodeXmlEntities(summaryMatch ? summaryMatch[1] : "")),
        date: updatedMatch ? updatedMatch[1] : "",
      });
    }
  } else {
    // RSS format
    const rssItems = body.match(/<item>[\s\S]*?<\/item>/g) || [];
    for (let i = 0; i < rssItems.length && i < count; i++) {
      const e = rssItems[i];
      const titleMatch = e.match(/<title[^>]*>([\s\S]*?)<\/title>/);
      const linkMatch = e.match(/<link[^>]*>([\s\S]*?)<\/link>/);
      const pubDateMatch = e.match(/<pubDate>([\s\S]*?)<\/pubDate>/);
      const descMatch = e.match(/<description[^>]*>([\s\S]*?)<\/description>/);
      let title = titleMatch ? titleMatch[1] : "";
      let desc = descMatch ? descMatch[1] : "";
      title = title.replace(/^<!\[CDATA\[/, "").replace(/\]\]>$/, "");
      desc = desc.replace(/^<!\[CDATA\[/, "").replace(/\]\]>$/, "");
      items.push({
        url: linkMatch ? linkMatch[1].trim() : "",
        title: decodeXmlEntities(title),
        summary: stripHtml(decodeXmlEntities(desc)),
        date: pubDateMatch ? pubDateMatch[1] : "",
      });
    }
  }
  return items;
}

// --- Reddit fetch with backoff on 429 ---

const REDDIT_BACKOFF_MIN = 60 * 60 * 1000;
const REDDIT_BACKOFF_MAX = 6 * 60 * 60 * 1000;

function redditBackoffKey(subreddit: string): string {
  return "reddit_backoff_" + subreddit;
}

function getRedditBackoff(subreddit: string): BackoffRecord | null {
  try {
    const raw = kv_get(redditBackoffKey(subreddit));
    if (raw) return JSON.parse(raw);
  } catch (_) {}
  return null;
}

function setRedditBackoff(subreddit: string): void {
  const existing = getRedditBackoff(subreddit);
  const wait = existing?.wait ? Math.min(existing.wait * 2, REDDIT_BACKOFF_MAX) : REDDIT_BACKOFF_MIN;
  kv_set(redditBackoffKey(subreddit), JSON.stringify({
    until: Date.now() + wait,
    wait,
  }));
}

function clearRedditBackoff(subreddit: string): void {
  kv_delete(redditBackoffKey(subreddit));
}

function fetchRedditRSS(subreddit: string, sort: string, count: number): ParsedItem[] {
  const url = "https://www.reddit.com/r/" + subreddit + "/" + sort + ".rss?limit=" + count;
  const resp = http_request("GET", url, {
    "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
    "Accept": "application/atom+xml",
  }, "");
  if (resp.status === 429) {
    setRedditBackoff(subreddit);
    const bo = getRedditBackoff(subreddit)!;
    const mins = Math.ceil(bo.wait / 60000);
    console.warn("Reddit r/" + subreddit + " rate limited (429), backing off for " + mins + "m");
    return [];
  }
  if (resp.status !== 200) {
    console.warn("Reddit RSS fetch failed for r/" + subreddit + ": status " + resp.status);
    return [];
  }
  clearRedditBackoff(subreddit);
  const entries = resp.body.match(/<entry>[\s\S]*?<\/entry>/g) || [];
  const items: ParsedItem[] = [];
  for (let i = 0; i < entries.length && i < count; i++) {
    const e = entries[i];
    const titleMatch = e.match(/<title>([\s\S]*?)<\/title>/);
    const linkMatch = e.match(/<link\s+href="([^"]+)"/);
    const updatedMatch = e.match(/<updated>([\s\S]*?)<\/updated>/);
    const contentMatch = e.match(/<content[^>]*>([\s\S]*?)<\/content>/);
    items.push({
      url: linkMatch ? linkMatch[1] : "",
      title: titleMatch ? decodeXmlEntities(titleMatch[1]) : "",
      summary: contentMatch ? stripHtml(decodeXmlEntities(contentMatch[1])) : "",
      score: 0,
      date: updatedMatch ? updatedMatch[1] : "",
    });
  }
  return items;
}

function fetchReddit(subreddit: string, sort: string, count: number): ParsedItem[] {
  const backoff = getRedditBackoff(subreddit);
  if (backoff && Date.now() < backoff.until) {
    const minsLeft = Math.ceil((backoff.until - Date.now()) / 60000);
    console.warn("Reddit r/" + subreddit + " on cooldown (" + minsLeft + "m remaining), skipping");
    return [];
  }
  return fetchRedditRSS(subreddit, sort, count);
}

// ========================================================================
// Main entry point (wrapped in IIFE for top-level return support)
// ========================================================================

(function main() {
  const globalCount: number = args.count || 0;
  const maxArticles: number = args.max_articles || 2;

  // Flatten categorized config into a single array
  const categories = Object.keys(cfg);
  const allSources: FeedSource[] = [];
  for (const cat of categories) {
    const items = cfg[cat] || [];
    if (!Array.isArray(items)) continue;
    for (const item of items) {
      item.category = cat;
      allSources.push(item);
    }
  }
  let feeds: FeedSource[] = allSources;

  // Filter to a specific feed or category if requested (fuzzy match).
  if (args.feed) {
    const query = args.feed;
    let matched = feeds.filter(f => f.name === query);
    if (matched.length === 0) {
      matched = feeds.filter(f => f.category === query.toLowerCase());
    }
    if (matched.length === 0) {
      const qWords = query.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim().split(/\s+/);
      matched = allSources.filter(f => {
        const fn = (f.name + " " + f.category).toLowerCase();
        return qWords.every(w => fn.indexOf(w) !== -1);
      });
    }
    if (matched.length === 0) {
      const names = allSources.map(f => f.name);
      return "No source matching '" + query + "'. Available: " + names.join(", ")
        + " | Categories: " + categories.join(", ");
    }
    feeds = matched;
  }

  if (feeds.length === 0) {
    return "No sources configured. Edit skills/scan_feeds/config.toml to add sources.";
  }

  // ========================================================================
  // Step 1: Fetch all configured feeds
  // ========================================================================

  const allItems: FeedItem[] = [];

  for (const feed of feeds) {
    const count = globalCount || feed.count || 5;
    const seenList = loadSeen(feed.name);
    const seenMap: Record<string, boolean> = {};
    for (const s of seenList) seenMap[s] = true;
    const newUrls: string[] = [];

    function addItem(url: string, title: string, summary: string, source: string, date: string): boolean {
      if (!url || seenMap[url]) return false;
      seenMap[url] = true;
      newUrls.push(url);
      allItems.push({
        feed: feed.name,
        category: feed.category || "unknown",
        title: title || "",
        link: url,
        summary: summary || "",
        source: source || extractDomain(url),
        date: date || "",
      });
      return true;
    }

    if (feed.type === "twitter") {
      const lists = feed.lists || [];
      const accounts = feed.accounts || [];
      const sources = lists.length + accounts.length;
      const perSource = sources > 0 ? Math.ceil(count / sources) : count;
      let added = 0;

      for (const listId of lists) {
        const items = fetchRsshub("/twitter/list/" + listId, perSource * FETCH_MULTIPLIER);
        for (const it of items) {
          if (added >= count) break;
          const url = it.url || it.id || "";
          const summary = it.content_text || stripHtml(it.content_html || "");
          if (addItem(url, it.title || "", summary, "x.com", it.date_published || "")) added++;
        }
      }

      for (const acct of accounts) {
        const handle = acct.replace(/^@/, "");
        const items = fetchRsshub("/twitter/user/" + handle, perSource * FETCH_MULTIPLIER);
        for (const it of items) {
          if (added >= count) break;
          const url = it.url || it.id || "";
          const summary = it.content_text || stripHtml(it.content_html || "");
          if (addItem(url, "@" + handle + ": " + (it.title || ""), summary, "x.com", it.date_published || "")) added++;
        }
      }

    } else if (feed.type === "reddit") {
      const items = fetchReddit(feed.subreddit || "", feed.sort || "hot", count * FETCH_MULTIPLIER);
      let added = 0;
      for (const it of items) {
        if (added >= count) break;
        if (addItem(it.url, it.title, it.summary, "reddit.com", it.date)) added++;
      }

    } else if (feed.type === "rsshub") {
      const items = fetchRsshub(feed.route!, count * FETCH_MULTIPLIER);
      let added = 0;
      for (const it of items) {
        if (added >= count) break;
        const url = it.url || it.id || "";
        const summary = it.content_text || stripHtml(it.content_html || "");
        if (addItem(url, it.title || "", summary, extractDomain(url), it.date_published || "")) added++;
      }

    } else if (feed.type === "rss") {
      const items = fetchDirectRSS(feed.url!, count * FETCH_MULTIPLIER);
      let added = 0;
      for (const it of items) {
        if (added >= count) break;
        if (addItem(it.url, it.title, it.summary, extractDomain(it.url), it.date)) added++;
      }
    }

    if (newUrls.length > 0) {
      saveSeen(feed.name, seenList.concat(newUrls));
    }
  }

  if (allItems.length === 0) return JSON.stringify({ count: 0, sources: [] });

  // ========================================================================
  // Step 2: Group items by feed name
  // ========================================================================

  const groups: Record<string, FeedItem[]> = {};
  const feedOrder: string[] = [];
  for (const item of allItems) {
    const feedName = item.feed || "unknown";
    if (!groups[feedName]) {
      groups[feedName] = [];
      feedOrder.push(feedName);
    }
    groups[feedName].push(item);
  }

  // ========================================================================
  // Step 3: Build delegation tasks — one per feed group
  // ========================================================================

  const tasks: { directive: string; context: string }[] = [];
  for (const name of feedOrder) {
    const items = groups[name];
    let directive = "Read up to " + maxArticles + " of the most important articles below "
      + "using read_url and summarize each in 2-3 sentences. ALWAYS include the article URL "
      + "with each summary. If read_url fails, note the article was unavailable.\n\n";
    directive += "Feed: " + name + "\n\n";
    for (let j = 0; j < items.length; j++) {
      const it = items[j];
      directive += (j + 1) + ". " + it.title + "\n   " + it.link + "\n";
      if (it.summary) {
        directive += "   Preview: " + it.summary + "\n";
      }
    }
    tasks.push({ directive, context: "" });
  }

  // ========================================================================
  // Step 4: Fan out with delegate_batch for parallel reading
  // ========================================================================

  const results = delegate_batch(tasks);

  // ========================================================================
  // Step 5: Collect results
  // ========================================================================

  const feedResults: FeedResult[] = [];
  for (let ri = 0; ri < feedOrder.length; ri++) {
    const feedName = feedOrder[ri];
    const r = results[ri];
    const category = (groups[feedName][0] || {} as FeedItem).category || "unknown";
    const entry: FeedResult = { source: feedName, category };
    if (r.error) {
      console.warn("delegate_batch failed for " + feedName + ": " + r.error);
      const fallback = groups[feedName].map(
        it => "- " + it.title + " — " + (it.summary || "") + " " + it.link
      );
      entry.summary = fallback.join("\n");
      entry.error = r.error;
    } else {
      entry.summary = r.result;
    }
    feedResults.push(entry);
  }

  return JSON.stringify({ count: feedResults.length, sources: feedResults });
})();
