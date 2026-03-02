// skill_config is a parsed TOML object: {accounts: [...], topics: [...]}
var cfg = skill_config || {};

// Args override config when provided; config is the fallback.
var accounts = args.accounts
    ? args.accounts.split(",").map(function(s) { return s.trim().replace(/^@/, ""); }).filter(function(s) { return s.length > 0; })
    : (cfg.accounts || []).slice();
var topics = args.topics
    ? args.topics.split(",").map(function(s) { return s.trim(); }).filter(function(s) { return s.length > 0; })
    : (cfg.topics || []).slice();
var count = args.count || 5;

if (accounts.length === 0 && topics.length === 0) {
    return "No accounts or topics configured. Edit skills/twitter-feed/config.toml or pass accounts/topics as arguments.";
}

// Load previously seen tweet URLs from persistent kv store.
// Stored as JSON array of URLs, pruned to last 200 to avoid unbounded growth.
var seenKey = "seen_urls";
var seenList = [];
try {
    var raw = kv_get(seenKey);
    if (raw) seenList = JSON.parse(raw);
} catch(e) { seenList = []; }

// Build a lookup dict from persisted list + this invocation.
var seen = {};
for (var k = 0; k < seenList.length; k++) {
    seen[seenList[k]] = true;
}

var results = [];
var newUrls = [];

function searchSearxng(query) {
    var url = "http://localhost:9000/search?format=json&time_range=day&q=" + encodeURIComponent(query);
    var resp = http_request("GET", url, {}, "");
    if (resp.status !== 200) return [];
    var data = JSON.parse(resp.body);
    return data.results || [];
}

function isStatusURL(u) {
    return u.indexOf("/status/") !== -1;
}

// Extract @handle from a status URL like https://x.com/karpathy/status/123
function extractAuthor(url) {
    var match = url.match(/x\.com\/([^/]+)\/status/);
    return match ? "@" + match[1] : "";
}

// Extract relative date from SearXNG content if present
function extractDate(content) {
    var match = content.match(/^(\d+ \w+ ago)\s/);
    return match ? match[1] : "";
}

// Clean up snippet text: strip leading relative date prefix
function cleanSnippet(content) {
    return (content || "").replace(/^\d+ \w+ ago\s*/, "").trim();
}

function addResult(h) {
    if (!h.url || !isStatusURL(h.url) || seen[h.url]) return false;
    seen[h.url] = true;
    newUrls.push(h.url);
    results.push({
        author: extractAuthor(h.url),
        link: h.url,
        summary: h.title || "",
        content: cleanSnippet(h.content),
        date: extractDate(h.content || "")
    });
    return true;
}

for (var i = 0; i < accounts.length; i++) {
    var handle = accounts[i].replace(/^@/, "");
    var hits = searchSearxng("site:x.com/" + handle + "/status");
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        if (addResult(hits[j])) added++;
    }
}

for (var i = 0; i < topics.length; i++) {
    var hits = searchSearxng("site:x.com " + topics[i]);
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        if (addResult(hits[j])) added++;
    }
}

// Persist newly seen URLs (append to list, cap at 200).
if (newUrls.length > 0) {
    var updated = seenList.concat(newUrls);
    if (updated.length > 200) {
        updated = updated.slice(updated.length - 200);
    }
    kv_set(seenKey, JSON.stringify(updated));
}

if (results.length === 0) return "No new tweets since last check.";

return {count: results.length, tweets: results};
