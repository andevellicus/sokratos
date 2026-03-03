// get-feeds: RSSHub-backed feed aggregator (Reddit, Twitter, HN, YouTube, etc.).
var cfg = skill_config || {};
var feeds = cfg.feeds || [];
var globalCount = args.count || 0;

// Filter to a specific feed if requested.
if (args.feed) {
    feeds = feeds.filter(function(f) { return f.name === args.feed; });
    if (feeds.length === 0) {
        return "No feed named '" + args.feed + "'. Available: " +
            (cfg.feeds || []).map(function(f) { return f.name; }).join(", ");
    }
}

if (feeds.length === 0) {
    return "No feeds configured. Edit skills/get-feeds/config.toml to add feeds.";
}

var rsshubBase = env("RSSHUB_URL") || "http://localhost:1200";
var FETCH_MULTIPLIER = 4; // over-fetch from source to compensate for dedup

// --- Dedup helpers (per-feed, persisted via kv_store) ---

function loadSeen(feedName) {
    var key = "seen_" + feedName;
    try {
        var raw = kv_get(key);
        if (raw) return JSON.parse(raw);
    } catch(e) {}
    return [];
}

function saveSeen(feedName, seenList) {
    var key = "seen_" + feedName;
    if (seenList.length > 200) {
        seenList = seenList.slice(seenList.length - 200);
    }
    kv_set(key, JSON.stringify(seenList));
}

// --- RSSHub fetch ---

function fetchRsshub(route, count) {
    var url = rsshubBase + route;
    var sep = url.indexOf("?") === -1 ? "?" : "&";
    url += sep + "limit=" + count + "&format=json";
    var resp = http_request("GET", url, {}, "");
    if (resp.status !== 200) {
        console.warn("RSSHub fetch failed for " + route + ": status " + resp.status);
        return [];
    }
    try {
        var data = JSON.parse(resp.body);
        return data.items || [];
    } catch(e) {
        console.warn("RSSHub parse failed for " + route + ": " + e);
        return [];
    }
}

function extractDomain(url) {
    var m = url.match(/^https?:\/\/(?:www\.)?([^\/]+)/);
    return m ? m[1] : "";
}

// --- Direct RSS/Atom fetch (for sites with native feeds) ---

function fetchDirectRSS(feedUrl, count) {
    var resp = http_request("GET", feedUrl, {
        "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
        "Accept": "application/rss+xml, application/atom+xml, application/xml"
    }, "");
    if (resp.status !== 200) {
        console.warn("Direct RSS fetch failed for " + feedUrl + ": status " + resp.status);
        return [];
    }
    var body = resp.body || "";
    var items = [];

    // Detect format: Atom uses <entry>, RSS 2.0 uses <item>.
    if (body.indexOf("<entry>") !== -1 || body.indexOf("<entry ") !== -1) {
        // Atom format (same parser as Reddit).
        var entries = body.match(/<entry[\s\S]*?<\/entry>/g) || [];
        for (var i = 0; i < entries.length && i < count; i++) {
            var e = entries[i];
            var titleMatch = e.match(/<title[^>]*>([\s\S]*?)<\/title>/);
            var linkMatch = e.match(/<link[^>]*href="([^"]+)"/);
            var updatedMatch = e.match(/<updated>([\s\S]*?)<\/updated>/);
            var summaryMatch = e.match(/<summary[^>]*>([\s\S]*?)<\/summary>/);
            items.push({
                url: linkMatch ? linkMatch[1] : "",
                title: decodeXmlEntities(titleMatch ? titleMatch[1] : ""),
                summary: stripHtml(decodeXmlEntities(summaryMatch ? summaryMatch[1] : "")),
                date: updatedMatch ? updatedMatch[1] : ""
            });
        }
    } else {
        // RSS 2.0 format.
        var rssItems = body.match(/<item>[\s\S]*?<\/item>/g) || [];
        for (var i = 0; i < rssItems.length && i < count; i++) {
            var e = rssItems[i];
            var titleMatch = e.match(/<title[^>]*>([\s\S]*?)<\/title>/);
            var linkMatch = e.match(/<link[^>]*>([\s\S]*?)<\/link>/);
            var pubDateMatch = e.match(/<pubDate>([\s\S]*?)<\/pubDate>/);
            var descMatch = e.match(/<description[^>]*>([\s\S]*?)<\/description>/);
            var title = titleMatch ? titleMatch[1] : "";
            var desc = descMatch ? descMatch[1] : "";
            // Strip CDATA wrappers.
            title = title.replace(/^<!\[CDATA\[/, "").replace(/\]\]>$/, "");
            desc = desc.replace(/^<!\[CDATA\[/, "").replace(/\]\]>$/, "");
            items.push({
                url: linkMatch ? linkMatch[1].trim() : "",
                title: decodeXmlEntities(title),
                summary: stripHtml(decodeXmlEntities(desc)),
                date: pubDateMatch ? pubDateMatch[1] : ""
            });
        }
    }
    return items;
}

function decodeXmlEntities(s) {
    return (s || "").replace(/&amp;/g,"&").replace(/&lt;/g,"<").replace(/&gt;/g,">").replace(/&#39;/g,"'").replace(/&quot;/g,'"');
}

// --- Reddit fetch (native JSON API) with backoff on 429 ---

var REDDIT_BACKOFF_MIN = 60 * 60 * 1000;  // 1 hour
var REDDIT_BACKOFF_MAX = 6 * 60 * 60 * 1000;  // 6 hours

function redditBackoffKey(subreddit) { return "reddit_backoff_" + subreddit; }

function getRedditBackoff(subreddit) {
    try {
        var raw = kv_get(redditBackoffKey(subreddit));
        if (raw) return JSON.parse(raw);
    } catch(e) {}
    return null;
}

function setRedditBackoff(subreddit) {
    var existing = getRedditBackoff(subreddit);
    var wait = REDDIT_BACKOFF_MIN;
    if (existing && existing.wait) {
        wait = Math.min(existing.wait * 2, REDDIT_BACKOFF_MAX);
    }
    kv_set(redditBackoffKey(subreddit), JSON.stringify({
        until: Date.now() + wait,
        wait: wait
    }));
}

function clearRedditBackoff(subreddit) {
    kv_delete(redditBackoffKey(subreddit));
}

// fetchRedditRSS uses Reddit's native Atom/RSS feed (no auth required).
function fetchRedditRSS(subreddit, sort, count) {
    var url = "https://www.reddit.com/r/" + subreddit + "/" + sort + ".rss?limit=" + count;
    var resp = http_request("GET", url, {
        "User-Agent": "sokratos:feeds:v1.0 (personal assistant bot)",
        "Accept": "application/atom+xml"
    }, "");
    if (resp.status === 429) {
        setRedditBackoff(subreddit);
        var bo = getRedditBackoff(subreddit);
        var mins = Math.ceil(bo.wait / 60000);
        console.warn("Reddit r/" + subreddit + " rate limited (429), backing off for " + mins + "m");
        return [];
    }
    if (resp.status !== 200) {
        console.warn("Reddit RSS fetch failed for r/" + subreddit + ": status " + resp.status);
        return [];
    }
    clearRedditBackoff(subreddit);
    // Parse Atom XML — extract <entry> elements via regex (goja has no DOM).
    var entries = resp.body.match(/<entry>[\s\S]*?<\/entry>/g) || [];
    var items = [];
    for (var i = 0; i < entries.length && i < count; i++) {
        var e = entries[i];
        var titleMatch = e.match(/<title>([\s\S]*?)<\/title>/);
        var linkMatch = e.match(/<link\s+href="([^"]+)"/);
        var updatedMatch = e.match(/<updated>([\s\S]*?)<\/updated>/);
        var contentMatch = e.match(/<content[^>]*>([\s\S]*?)<\/content>/);
        var title = titleMatch ? titleMatch[1].replace(/&amp;/g,"&").replace(/&lt;/g,"<").replace(/&gt;/g,">").replace(/&#39;/g,"'").replace(/&quot;/g,'"') : "";
        var link = linkMatch ? linkMatch[1] : "";
        var date = updatedMatch ? updatedMatch[1] : "";
        var summary = contentMatch ? stripHtml(contentMatch[1].replace(/&lt;/g,"<").replace(/&gt;/g,">").replace(/&amp;/g,"&")) : "";
        items.push({ url: link, title: title, summary: summary, score: 0, date: date });
    }
    return items;
}

function fetchReddit(subreddit, sort, count) {
    // Check backoff before hitting the API.
    var backoff = getRedditBackoff(subreddit);
    if (backoff && Date.now() < backoff.until) {
        var minsLeft = Math.ceil((backoff.until - Date.now()) / 60000);
        console.warn("Reddit r/" + subreddit + " on cooldown (" + minsLeft + "m remaining), skipping");
        return [];
    }

    // Use Reddit's native RSS feed (no auth required, unlike the JSON API).
    return fetchRedditRSS(subreddit, sort, count);
}

function stripHtml(html) {
    return (html || "").replace(/<[^>]+>/g, "").substring(0, 300);
}

// --- Main: iterate feeds ---

var allItems = [];

for (var i = 0; i < feeds.length; i++) {
    var feed = feeds[i];
    var count = globalCount || feed.count || 5;
    var seenList = loadSeen(feed.name);
    var seenMap = {};
    for (var k = 0; k < seenList.length; k++) seenMap[seenList[k]] = true;
    var newUrls = [];

    function addItem(url, title, summary, source, date) {
        if (!url || seenMap[url]) return false;
        seenMap[url] = true;
        newUrls.push(url);
        allItems.push({
            feed: feed.name,
            title: title || "",
            link: url,
            summary: summary || "",
            source: source || extractDomain(url),
            date: date || ""
        });
        return true;
    }

    if (feed.type === "twitter") {
        // Twitter via RSSHub: fetch from lists and/or per-account routes.
        // Distribute count evenly across all sources, then cap at total count.
        var lists = feed.lists || [];
        var accounts = feed.accounts || [];
        var sources = lists.length + accounts.length;
        var perSource = sources > 0 ? Math.ceil(count / sources) : count;
        var added = 0;

        for (var li = 0; li < lists.length; li++) {
            var items = fetchRsshub("/twitter/list/" + lists[li], perSource * FETCH_MULTIPLIER);
            for (var j = 0; j < items.length && added < count; j++) {
                var it = items[j];
                var url = it.url || it.id || "";
                var summary = it.content_text || stripHtml(it.content_html);
                if (addItem(url, it.title, summary, "x.com", it.date_published || "")) {
                    added++;
                }
            }
        }

        for (var ai = 0; ai < accounts.length; ai++) {
            var handle = accounts[ai].replace(/^@/, "");
            var items = fetchRsshub("/twitter/user/" + handle, perSource * FETCH_MULTIPLIER);
            for (var j = 0; j < items.length && added < count; j++) {
                var it = items[j];
                var url = it.url || it.id || "";
                var summary = it.content_text || stripHtml(it.content_html);
                var author = "@" + handle;
                if (addItem(url, author + ": " + (it.title || ""), summary, "x.com", it.date_published || "")) {
                    added++;
                }
            }
        }

    } else if (feed.type === "reddit") {
        // Reddit native JSON API.
        var items = fetchReddit(feed.subreddit || "", feed.sort || "hot", count * FETCH_MULTIPLIER);
        var added = 0;
        for (var j = 0; j < items.length && added < count; j++) {
            var it = items[j];
            if (addItem(it.url, it.title, it.summary, "reddit.com", it.date)) {
                added++;
            }
        }

    } else if (feed.type === "rsshub") {
        // Generic RSSHub feed.
        var items = fetchRsshub(feed.route, count * FETCH_MULTIPLIER);
        var added = 0;
        for (var j = 0; j < items.length && added < count; j++) {
            var it = items[j];
            var url = it.url || it.id || "";
            var summary = it.content_text || stripHtml(it.content_html);
            if (addItem(url, it.title, summary, extractDomain(url), it.date_published || "")) {
                added++;
            }
        }

    } else if (feed.type === "rss") {
        // Direct RSS/Atom feed (no RSSHub proxy).
        var items = fetchDirectRSS(feed.url, count * FETCH_MULTIPLIER);
        var added = 0;
        for (var j = 0; j < items.length && added < count; j++) {
            var it = items[j];
            if (addItem(it.url, it.title, it.summary, extractDomain(it.url), it.date)) {
                added++;
            }
        }
    }

    // Persist newly seen URLs for this feed.
    if (newUrls.length > 0) {
        saveSeen(feed.name, seenList.concat(newUrls));
    }
}

if (allItems.length === 0) return "No new items across feeds.";

// Format as readable text with links so the orchestrator preserves them.
var lines = [];
var currentFeed = "";
for (var i = 0; i < allItems.length; i++) {
    var it = allItems[i];
    if (it.feed !== currentFeed) {
        if (lines.length > 0) lines.push("");
        lines.push("## " + it.feed);
        currentFeed = it.feed;
    }
    var line = "- " + it.title;
    if (it.summary) line += " — " + it.summary;
    line += "\n  " + it.link;
    lines.push(line);
}
return allItems.length + " new item(s):\n\n" + lines.join("\n");
