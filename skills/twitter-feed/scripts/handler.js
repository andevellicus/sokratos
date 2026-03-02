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

var seen = {};
var results = [];

function searchSearxng(query) {
    var url = "http://localhost:9000/search?format=json&time_range=week&q=" + encodeURIComponent(query);
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

for (var i = 0; i < accounts.length; i++) {
    var handle = accounts[i].replace(/^@/, "");
    var hits = searchSearxng("site:x.com/" + handle + "/status");
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        var h = hits[j];
        if (h.url && isStatusURL(h.url) && !seen[h.url]) {
            seen[h.url] = true;
            results.push({
                author: extractAuthor(h.url),
                text: h.title || "",
                snippet: h.content || "",
                url: h.url,
                date: extractDate(h.content || "")
            });
            added++;
        }
    }
}

for (var i = 0; i < topics.length; i++) {
    var hits = searchSearxng("site:x.com " + topics[i]);
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        var h = hits[j];
        if (h.url && isStatusURL(h.url) && !seen[h.url]) {
            seen[h.url] = true;
            results.push({
                author: extractAuthor(h.url),
                text: h.title || "",
                snippet: h.content || "",
                url: h.url,
                date: extractDate(h.content || "")
            });
            added++;
        }
    }
}

if (results.length === 0) return "No tweets found.";

return {count: results.length, tweets: results};
