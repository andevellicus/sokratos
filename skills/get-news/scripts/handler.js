// Parse config.txt: [sources] and [topics] sections, one entry per line.
function parseConfig(raw) {
    var sources = [];
    var topics = [];
    if (!raw) return {sources: sources, topics: topics};
    var section = "";
    var lines = raw.split("\n");
    for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line || line.charAt(0) === "#") continue;
        if (line === "[sources]") { section = "sources"; continue; }
        if (line === "[topics]") { section = "topics"; continue; }
        if (section === "sources") sources.push(line);
        if (section === "topics") topics.push(line);
    }
    return {sources: sources, topics: topics};
}

var cfg = parseConfig(skill_config);

// Config is always the base; args add extra sources/topics on top.
var sources = cfg.sources.slice();
if (args.sources) {
    var extra = args.sources.split(",").map(function(s) { return s.trim(); }).filter(function(s) { return s.length > 0; });
    for (var i = 0; i < extra.length; i++) {
        if (sources.indexOf(extra[i]) === -1) sources.push(extra[i]);
    }
}
var topics = cfg.topics.slice();
if (args.topics) {
    var extra = args.topics.split(",").map(function(s) { return s.trim(); }).filter(function(s) { return s.length > 0; });
    for (var i = 0; i < extra.length; i++) {
        if (topics.indexOf(extra[i]) === -1) topics.push(extra[i]);
    }
}
var count = args.count || 5;

if (topics.length === 0) {
    return "No topics configured. Edit skills/get-news/config.txt or pass topics as arguments.";
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
            results.push({title: h.title || "", url: h.url, snippet: h.content || ""});
            added++;
        }
    }
}

if (results.length === 0) return "No news articles found.";

var lines = ["Found " + results.length + " articles (ALWAYS include the URL for each article in your response):\n"];
for (var k = 0; k < results.length; k++) {
    var r = results[k];
    lines.push((k + 1) + ". " + r.title);
    lines.push("   " + r.snippet);
    lines.push("   " + r.url);
    lines.push("");
}
return lines.join("\n");
