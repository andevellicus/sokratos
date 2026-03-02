// skill_config is a parsed TOML object: {location: "..."}
var cfg = skill_config || {};
var loc = args.location || cfg.location || "New York";
var resp = http_request("GET", "https://wttr.in/" + encodeURIComponent(loc) + "?format=j1", {}, "");
if (resp.status !== 200) return "Weather service error: HTTP " + resp.status;
var data = JSON.parse(resp.body);
var cur = data.current_condition[0];

var current = {
    condition: cur.weatherDesc[0].value,
    temp_f: cur.temp_F,
    temp_c: cur.temp_C,
    humidity: cur.humidity,
    wind_mph: cur.windspeedMiles,
    feels_like_f: cur.FeelsLikeF
};

var forecast = [];
var weather = data.weather || [];
for (var i = 0; i < weather.length && i < 3; i++) {
    var day = weather[i];
    // Use the midday hourly entry (index 4 = noon) for the day's condition.
    var midday = day.hourly && day.hourly[4] ? day.hourly[4] : {};
    forecast.push({
        date: day.date,
        high_f: day.maxtempF,
        low_f: day.mintempF,
        high_c: day.maxtempC,
        low_c: day.mintempC,
        condition: midday.weatherDesc ? midday.weatherDesc[0].value : ""
    });
}

return {location: loc, current: current, forecast: forecast};
