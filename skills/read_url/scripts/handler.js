var resp = http_request("GET", args.url, {
  "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
});

if (resp.status !== 200) {
  "HTTP " + resp.status + ": failed to fetch URL";
} else {
  var text = resp.body;
  // Strip script and style blocks.
  text = text.replace(/<script[\s\S]*?<\/script>/gi, "");
  text = text.replace(/<style[\s\S]*?<\/style>/gi, "");
  // Strip all HTML tags.
  text = text.replace(/<[^>]+>/g, " ");
  // Decode common HTML entities.
  text = text.replace(/&amp;/g, "&");
  text = text.replace(/&lt;/g, "<");
  text = text.replace(/&gt;/g, ">");
  text = text.replace(/&quot;/g, '"');
  text = text.replace(/&#39;/g, "'");
  text = text.replace(/&nbsp;/g, " ");
  // Collapse whitespace.
  text = text.replace(/\s+/g, " ").trim();
  // Truncate to 2000 chars.
  if (text.length > 2000) {
    text = text.substring(0, 2000) + "\n... (truncated)";
  }
  text;
}
