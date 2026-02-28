var loc = args.location || "Greenville, SC";
var resp = http_request("GET", "https://wttr.in/" + encodeURIComponent(loc) + "?format=j1", {}, "");
if (resp.status !== 200) return "Weather service error: HTTP " + resp.status;
var data = JSON.parse(resp.body);
var cur = data.current_condition[0];
return {condition: cur.weatherDesc[0].value, temperature: cur.temp_F + "F", humidity: cur.humidity + "%"};