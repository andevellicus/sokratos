var url = "http://localhost:9000/search?q=" + encodeURIComponent(args.query) + "&format=json";
var resp = http_request("GET", url);

if (resp.status !== 200) {
  "Search failed with status " + resp.status + ": " + resp.body;
} else {
  var data = JSON.parse(resp.body);
  var results = data.results || [];
  if (results.length === 0) {
    "No results found.";
  } else {
    var limit = Math.min(5, results.length);
    var out = "";
    for (var i = 0; i < limit; i++) {
      var r = results[i];
      out += (i + 1) + ". " + r.title + "\n   " + r.url + "\n   " + (r.content || "") + "\n\n";
    }
    out;
  }
}
