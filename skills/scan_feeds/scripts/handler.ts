// scan_feeds: Fetches configured feeds, deduplicates, then dispatches parallel
// subagents to read and summarize top articles per feed group.

declare const args: { count?: number; feed?: string; max_articles?: number };
declare const skill_config: Record<string, any[]> | undefined;
declare function http_request(method: string, url: string, headers: Record<string, string>, body: string): { status: number; body: string; headers: Record<string, string> };
declare function http_batch(requests: { method: string; url: string; headers?: Record<string, string>; body?: string }[]): { status: number; body: string; headers: Record<string, string>; error?: string }[];
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

// A pending fetch request: the URL to fetch + metadata needed to parse results.
interface FetchJob {
  feedName: string;
  feedType: string;
  url: string;
  headers: Record<string, string>;
  count: number;
  // twitter-specific
  twitterHandle?: string;
}

const cfg = skill_config || {};
const rsshubBase: string = env("RSSHUB_URL") || "http://localhost:1200";
const FETCH_MULTIPLIER = 4;
const settings = (cfg as any).settings || {};
const MAX_AGE_MS = ((settings.max_age_hours as number) || 48) * 60 * 60 * 1000;

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

// --- Recency filter ---

function isTooOld(dateStr: string): boolean {
  if (!dateStr) return false; // no date → fail-open, include it
  const ts = new Date(dateStr).getTime();
  if (isNaN(ts)) return false; // unparseable → fail-open
  return (Date.now() - ts) > MAX_AGE_MS;
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

// --- Parsing helpers (no HTTP, just parse response bodies) ---

function parseRsshubJson(body: string): RsshubItem[] {
  try {
    const data = JSON.parse(body);
    return data.items || [];
  } catch (e) {
    return [];
  }
}

function parseAtomOrRss(body: string, count: number): ParsedItem[] {
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

function parseRedditAtom(body: string, count: number): ParsedItem[] {
  const entries = body.match(/<entry>[\s\S]*?<\/entry>/g) || [];
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

// --- Reddit backoff ---

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

function buildRsshubUrl(route: string, count: number): string {
  let url = rsshubBase + route;
  const sep = url.indexOf("?") === -1 ? "?" : "&";
  return url + sep + "limit=" + count + "&format=json";
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
  // Step 1: Build fetch jobs — collect all URLs needed across all feeds
  // ========================================================================

  const fetchJobs: FetchJob[] = [];

  for (const feed of feeds) {
    const count = globalCount || feed.count || 5;

    if (feed.type === "twitter") {
      const lists = feed.lists || [];
      const accounts = feed.accounts || [];
      const sources = lists.length + accounts.length;
      const perSource = sources > 0 ? Math.ceil(count / sources) : count;

      for (const listId of lists) {
        fetchJobs.push({
          feedName: feed.name,
          feedType: "rsshub",
          url: buildRsshubUrl("/twitter/list/" + listId, perSource * FETCH_MULTIPLIER),
          headers: {},
          count: count,
          twitterHandle: undefined,
        });
      }
      for (const acct of accounts) {
        const handle = acct.replace(/^@/, "");
        fetchJobs.push({
          feedName: feed.name,
          feedType: "rsshub",
          url: buildRsshubUrl("/twitter/user/" + handle, perSource * FETCH_MULTIPLIER),
          headers: {},
          count: count,
          twitterHandle: handle,
        });
      }

    } else if (feed.type === "reddit") {
      const subreddit = feed.subreddit || "";
      const backoff = getRedditBackoff(subreddit);
      if (backoff && Date.now() < backoff.until) {
        const minsLeft = Math.ceil((backoff.until - Date.now()) / 60000);
        console.warn("Reddit r/" + subreddit + " on cooldown (" + minsLeft + "m remaining), skipping");
        continue;
      }
      fetchJobs.push({
        feedName: feed.name,
        feedType: "reddit",
        url: "https://www.reddit.com/r/" + subreddit + "/" + (feed.sort || "hot") + ".rss?limit=" + (count * FETCH_MULTIPLIER),
        headers: {
          "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
          "Accept": "application/atom+xml",
        },
        count: count,
      });

    } else if (feed.type === "rsshub") {
      fetchJobs.push({
        feedName: feed.name,
        feedType: "rsshub",
        url: buildRsshubUrl(feed.route!, count * FETCH_MULTIPLIER),
        headers: {},
        count: count,
      });

    } else if (feed.type === "rss") {
      fetchJobs.push({
        feedName: feed.name,
        feedType: "rss",
        url: feed.url!,
        headers: {
          "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
          "Accept": "application/rss+xml, application/atom+xml, application/xml",
        },
        count: count * FETCH_MULTIPLIER,
      });
    }
  }

  // ========================================================================
  // Step 2: Batch-fetch all URLs in parallel
  // ========================================================================

  const batchRequests = fetchJobs.map(j => ({
    method: "GET",
    url: j.url,
    headers: j.headers,
  }));

  const batchResponses = batchRequests.length > 0 ? http_batch(batchRequests) : [];

  // ========================================================================
  // Step 3: Parse responses and build items per feed
  // ========================================================================

  const allItems: FeedItem[] = [];
  const pendingSeen: Record<string, { seenList: string[]; newUrls: string[] }> = {};

  // Pre-load seen lists.
  for (const feed of feeds) {
    if (!pendingSeen[feed.name]) {
      const seenList = loadSeen(feed.name);
      pendingSeen[feed.name] = { seenList, newUrls: [] };
    }
  }

  // Track added count per feed name for twitter multi-source feeds.
  const feedAdded: Record<string, number> = {};
  const feedMaxCount: Record<string, number> = {};
  for (const feed of feeds) {
    feedMaxCount[feed.name] = globalCount || feed.count || 5;
    feedAdded[feed.name] = 0;
  }

  for (let i = 0; i < fetchJobs.length; i++) {
    const job = fetchJobs[i];
    const resp = batchResponses[i];
    const maxCount = feedMaxCount[job.feedName] || 5;
    const ps = pendingSeen[job.feedName];
    const seenMap: Record<string, boolean> = {};
    for (const s of ps.seenList) seenMap[s] = true;
    // Also mark already-added URLs as seen (for multi-source feeds like twitter).
    for (const u of ps.newUrls) seenMap[u] = true;

    function addItem(url: string, title: string, summary: string, source: string, date: string): boolean {
      if (feedAdded[job.feedName] >= maxCount) return false;
      if (!url || seenMap[url]) return false;
      if (isTooOld(date)) return false;
      seenMap[url] = true;
      ps.newUrls.push(url);
      allItems.push({
        feed: job.feedName,
        category: feeds.find(f => f.name === job.feedName)?.category || "unknown",
        title: title || "",
        link: url,
        summary: summary || "",
        source: source || extractDomain(url),
        date: date || "",
      });
      feedAdded[job.feedName]++;
      return true;
    }

    if (resp.error) {
      console.warn("Fetch failed for " + job.url + ": " + resp.error);
      continue;
    }

    if (job.feedType === "reddit") {
      if (resp.status === 429) {
        // Extract subreddit from URL for backoff.
        const subMatch = job.url.match(/\/r\/([^\/]+)/);
        if (subMatch) {
          setRedditBackoff(subMatch[1]);
          console.warn("Reddit r/" + subMatch[1] + " rate limited (429), backing off");
        }
        continue;
      }
      if (resp.status !== 200) {
        console.warn("Reddit fetch failed: status " + resp.status);
        continue;
      }
      // Clear backoff on success.
      const subMatch = job.url.match(/\/r\/([^\/]+)/);
      if (subMatch) clearRedditBackoff(subMatch[1]);

      const items = parseRedditAtom(resp.body, job.count);
      for (const it of items) {
        addItem(it.url, it.title, it.summary, "reddit.com", it.date);
      }

    } else if (job.feedType === "rsshub") {
      if (resp.status !== 200) {
        console.warn("RSSHub fetch failed for " + job.url + ": status " + resp.status);
        continue;
      }
      const items = parseRsshubJson(resp.body);
      for (const it of items) {
        const url = it.url || it.id || "";
        const summary = it.content_text || stripHtml(it.content_html || "");
        const titlePrefix = job.twitterHandle ? "@" + job.twitterHandle + ": " : "";
        const source = job.twitterHandle ? "x.com" : extractDomain(url);
        addItem(url, titlePrefix + (it.title || ""), summary, source, it.date_published || "");
      }

    } else if (job.feedType === "rss") {
      if (resp.status !== 200) {
        console.warn("RSS fetch failed for " + job.url + ": status " + resp.status);
        continue;
      }
      const items = parseAtomOrRss(resp.body, job.count);
      for (const it of items) {
        addItem(it.url, it.title, it.summary, extractDomain(it.url), it.date);
      }
    }
  }

  if (allItems.length === 0) return JSON.stringify({ count: 0, sources: [] });

  // ========================================================================
  // Step 4: Group items by feed name
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
  // Step 5: Build delegation tasks — one per feed group
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
  // Step 6: Fan out with delegate_batch for parallel reading
  // ========================================================================

  const results = delegate_batch(tasks);

  // ========================================================================
  // Step 7: Collect results
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
      // Don't mark seen — delegate failed, items can be retried next run.
    } else {
      entry.summary = r.result;
      // Delegate succeeded — mark all items from this feed as seen.
      const ps = pendingSeen[feedName];
      if (ps && ps.newUrls.length > 0) {
        saveSeen(feedName, ps.seenList.concat(ps.newUrls));
      }
    }
    feedResults.push(entry);
  }

  return JSON.stringify({ count: feedResults.length, sources: feedResults });
})();
