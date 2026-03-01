// Read default location from config.txt, fall back to args or "New York"
var defaultLoc = (skill_config || "").split("\n").filter(function(l) {
    var t = l.trim();
    return t.length > 0 && t.charAt(0) !== "#";
})[0] || "New York";

var loc = args.location || defaultLoc;
var resp = http_request("GET", "https://wttr.in/" + encodeURIComponent(loc) + "?format=j1", {}, "");
if (resp.status !== 200) return "Weather service error: HTTP " + resp.status;
var data = JSON.parse(resp.body);
var cur = data.current_condition[0];
return {location: loc, condition: cur.weatherDesc[0].value, temperature: cur.temp_F + "F", humidity: cur.humidity + "%"};
