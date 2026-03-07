// get_weather: Geocodes a location, fetches current weather + 3-day forecast
// from Open-Meteo (free, no API key), and returns structured JSON.

declare const args: { location?: string };
declare const skill_config: { location?: string } | undefined;
declare function http_request(method: string, url: string, headers: Record<string, string>, body: string): { status: number; body: string; headers: Record<string, string> };

interface GeoResult {
  latitude: number;
  longitude: number;
  name: string;
  admin1?: string;
  country?: string;
  country_code?: string;
}

interface CurrentWeather {
  condition: string;
  temp_f: number;
  feels_like_f: number;
  humidity: number;
  wind_mph: number;
}

interface ForecastDay {
  date: string;
  high_f: number;
  low_f: number;
  precip_chance: number;
  condition: string;
}

// WMO weather code to human-readable description.
const WMO: Record<number, string> = {
  0: "Clear sky", 1: "Mainly clear", 2: "Partly cloudy", 3: "Overcast",
  45: "Fog", 48: "Rime fog",
  51: "Light drizzle", 53: "Moderate drizzle", 55: "Dense drizzle",
  56: "Light freezing drizzle", 57: "Dense freezing drizzle",
  61: "Light rain", 63: "Moderate rain", 65: "Heavy rain",
  66: "Light freezing rain", 67: "Heavy freezing rain",
  71: "Light snow", 73: "Moderate snow", 75: "Heavy snow", 77: "Snow grains",
  80: "Light rain showers", 81: "Moderate rain showers", 82: "Violent rain showers",
  85: "Light snow showers", 86: "Heavy snow showers",
  95: "Thunderstorm", 96: "Thunderstorm with hail", 99: "Thunderstorm with heavy hail",
};

(function main() {
  const cfg = skill_config || {};
  const settings = (cfg as any).settings || cfg;
  const loc: string = args.location || settings.location || "New York";
  const FORECAST_DAYS = settings.forecast_days || 3;
  const GEOCODE_RESULTS = settings.geocode_results || 5;

  // Step 1: Geocode the location name to lat/lon.
  // Open-Meteo geocoding fails on "City, State" format — strip qualifier and
  // use it to filter results when the bare city name returns multiple matches.
  let qualifier = "";
  let searchName = loc;
  const commaIdx = loc.indexOf(",");
  if (commaIdx !== -1) {
    searchName = loc.substring(0, commaIdx).trim();
    qualifier = loc.substring(commaIdx + 1).trim().toLowerCase();
  }

  const geoResp = http_request(
    "GET",
    "https://geocoding-api.open-meteo.com/v1/search?name=" + encodeURIComponent(searchName) + "&count=" + GEOCODE_RESULTS + "&language=en",
    {}, "",
  );
  if (geoResp.status !== 200) return "Geocoding error: HTTP " + geoResp.status;

  const geoData: { results?: GeoResult[] } = JSON.parse(geoResp.body);
  if (!geoData.results || geoData.results.length === 0) return "Location not found: " + loc;

  // If a qualifier was given (e.g. "SC", "South Carolina"), match it against
  // admin1 (state/region name). Handles both full names and abbreviations by
  // building initials from admin1 words (e.g. "South Carolina" -> "sc").
  let place: GeoResult = geoData.results[0];
  if (qualifier) {
    for (const r of geoData.results) {
      const a1 = (r.admin1 || "").toLowerCase();
      const initials = a1.split(/\s+/).filter(Boolean).map(w => w[0]).join("");
      if (
        a1 === qualifier ||
        a1.startsWith(qualifier) ||
        initials === qualifier ||
        (r.country_code || "").toLowerCase() === qualifier
      ) {
        place = r;
        break;
      }
    }
  }

  const resolvedName = place.name
    + (place.admin1 ? ", " + place.admin1 : "")
    + (place.country ? ", " + place.country : "");

  // Step 2: Fetch weather from Open-Meteo (free, no API key).
  const wxUrl = "https://api.open-meteo.com/v1/forecast"
    + "?latitude=" + place.latitude + "&longitude=" + place.longitude
    + "&current=temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,wind_speed_10m,wind_direction_10m"
    + "&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max"
    + "&temperature_unit=fahrenheit&wind_speed_unit=mph&forecast_days=" + FORECAST_DAYS + "&timezone=auto";

  const wxResp = http_request("GET", wxUrl, {}, "");
  if (wxResp.status !== 200) return "Weather API error: HTTP " + wxResp.status;

  const wx = JSON.parse(wxResp.body);
  const cur = wx.current;

  const current: CurrentWeather = {
    condition: WMO[cur.weather_code] || "Unknown (" + cur.weather_code + ")",
    temp_f: Math.round(cur.temperature_2m),
    feels_like_f: Math.round(cur.apparent_temperature),
    humidity: cur.relative_humidity_2m,
    wind_mph: Math.round(cur.wind_speed_10m),
  };

  const daily = wx.daily;
  const forecast: ForecastDay[] = [];
  for (let i = 0; i < daily.time.length; i++) {
    forecast.push({
      date: daily.time[i],
      high_f: Math.round(daily.temperature_2m_max[i]),
      low_f: Math.round(daily.temperature_2m_min[i]),
      precip_chance: daily.precipitation_probability_max[i],
      condition: WMO[daily.weather_code[i]] || "Unknown",
    });
  }

  return { location: resolvedName, current, forecast };
})();
