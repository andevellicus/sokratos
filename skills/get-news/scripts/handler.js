// skill_config is a parsed TOML object: {sources: [...], topics: [...]}
var cfg = skill_config || {};

// Args override config when provided; config is the fallback.
var sources = args.sources
    ? args.sources.split(",").map(function(s) { return s.trim(); }).filter(function(s) { return s.length > 0; })
    : (cfg.sources || []).slice();
var topics = args.topics
    ? args.topics.split(",").map(function(s) { return s.trim(); }).filter(function(s) { return s.length > 0; })
    : (cfg.topics || []).slice();
var count = args.count || 5;

if (topics.length === 0) {
    return "No topics configured. Edit skills/get-news/config.toml or pass topics as arguments.";
}

var seen = {};
var results = [];

function searchSearxng(query) {
    var url = "http://localhost:9000/search?format=json&q=" + encodeURIComponent(query);
    var resp = http_request("GET", url, {}, "");
    if (resp.status !== 200) return [];
    var data = JSON.parse(resp.body);
    return data.results || [];
}

for (var i = 0; i < topics.length; i++) {
    // Build site filter if sources are configured
    var siteFilter = "";
    if (sources.length > 0) {
        // SearXNG supports multiple site: filters with OR
        siteFilter = " (" + sources.map(function(s) { return "site:" + s; }).join(" OR ") + ")";
    }
    var query = topics[i] + siteFilter;
    var hits = searchSearxng(query);
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        var h = hits[j];
        if (h.url && !seen[h.url]) {
            seen[h.url] = true;
            // Extract source domain from URL.
            var source = "";
            var match = h.url.match(/^https?:\/\/(?:www\.)?([^\/]+)/);
            if (match) source = match[1];
            results.push({
                title: h.title || "",
                url: h.url,
                snippet: h.content || "",
                source: source,
                date: h.publishedDate || ""
            });
            added++;
        }
    }
}

if (results.length === 0) return "No news articles found.";

return {count: results.length, articles: results};
