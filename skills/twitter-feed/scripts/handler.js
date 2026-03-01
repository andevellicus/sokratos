// Parse config.txt: [accounts] and [topics] sections, one entry per line.
// Args override config when provided.
function parseConfig(raw) {
    var accounts = [];
    var topics = [];
    if (!raw) return {accounts: accounts, topics: topics};
    var section = "";
    var lines = raw.split("\n");
    for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line || line.charAt(0) === "#") continue;
        if (line === "[accounts]") { section = "accounts"; continue; }
        if (line === "[topics]") { section = "topics"; continue; }
        if (section === "accounts") accounts.push(line.replace(/^@/, ""));
        if (section === "topics") topics.push(line);
    }
    return {accounts: accounts, topics: topics};
}

var cfg = parseConfig(skill_config);

// Config is always the base; args add extra accounts/topics on top.
var accounts = cfg.accounts.slice();
if (args.accounts) {
    var extra = args.accounts.split(",").map(function(s) { return s.trim().replace(/^@/, ""); }).filter(function(s) { return s.length > 0; });
    for (var i = 0; i < extra.length; i++) {
        if (accounts.indexOf(extra[i]) === -1) accounts.push(extra[i]);
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

if (accounts.length === 0 && topics.length === 0) {
    return "No accounts or topics configured. Edit skills/twitter-feed/config.txt or pass accounts/topics as arguments.";
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

for (var i = 0; i < accounts.length; i++) {
    var handle = accounts[i].replace(/^@/, "");
    var hits = searchSearxng("site:x.com/" + handle + "/status");
    var added = 0;
    for (var j = 0; j < hits.length && added < count; j++) {
        var h = hits[j];
        if (h.url && isStatusURL(h.url) && !seen[h.url]) {
            seen[h.url] = true;
            results.push({title: h.title || "", url: h.url, snippet: h.content || ""});
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
            results.push({title: h.title || "", url: h.url, snippet: h.content || ""});
            added++;
        }
    }
}

if (results.length === 0) return "No tweets found.";

var lines = ["Found " + results.length + " tweets (ALWAYS include the URL for each tweet in your response):\n"];
for (var k = 0; k < results.length; k++) {
    var r = results[k];
    lines.push((k + 1) + ". " + r.title);
    lines.push("   " + r.snippet);
    lines.push("   " + r.url);
    lines.push("");
}
return lines.join("\n");
