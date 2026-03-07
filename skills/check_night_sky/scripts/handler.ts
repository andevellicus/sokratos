/* eslint-disable no-unused-vars */
/* eslint-disable @typescript-eslint/no-unused-vars */
/* eslint-disable no-var */
/* eslint-disable @typescript-eslint/no-explicit-any */

// Type declarations for runtime globals
declare const args: any;
declare const skill_config: any;
declare const http_request: (method: string, url: string, headers: any, body: string) => { status: number, body: string, headers: object };
declare const console: { log(...args: any[]): void, warn(...args: any[]): void, error(...args: any[]): void };
declare const call_tool: (name: string, args: any) => string;
declare const sleep: (ms: number) => void;
declare const hash_sha256: (s: string) => string;
declare const hash_hmac_sha256: (key: string, msg: string) => string;
declare const btoa: (s: string) => string;
declare const atob: (s: string) => string;

interface WeatherResponse {
  latitude: number;
  longitude: number;
  timezone: string;
  current: {
    time: string;
    cloud_cover: number;
  };
  hourly: {
    time: string[];
    cloud_cover: number[];
  };
}

interface GeoResult {
  results: Array<{
    id: number;
    name: string;
    latitude: number;
    longitude: number;
  }>;
}

function calculateSkyQuality(cloudCover: number, humidity: number, windSpeed: number): number {
  // Returns a score from 0-100 (100 = perfect conditions)
  let score = 100;
  
  // Cloud cover impact (60% weight)
  score -= cloudCover * 0.6;
  
  // Humidity impact (20% weight) - high humidity risks dew
  if (humidity > 70) {
    score -= (humidity - 70) * 0.4;
  }
  
  // Wind speed impact (20% weight) - optimal 5-10 km/h
  if (windSpeed < 5) {
    score -= (5 - windSpeed) * 0.8;
  } else if (windSpeed > 10) {
    score -= (windSpeed - 10) * 0.8;
  }
  
  return Math.max(0, Math.min(100, Math.round(score)));
}

function formatCondition(score: number): { label: string, color: string, recommendation: string } {
  if (score >= 90) {
    return { label: 'Excellent', color: 'green', recommendation: 'Perfect conditions for astrophotography' };
  } else if (score >= 80) {
    return { label: 'Good', color: 'lightgreen', recommendation: 'Good conditions, minor limitations' };
  } else if (score >= 50) {
    return { label: 'Marginal', color: 'yellow', recommendation: 'Marginal conditions, consider alternatives' };
  } else {
    return { label: 'Poor', color: 'red', recommendation: 'Poor conditions, not recommended for astrophotography' };
  }
}

function main(): string {
  try {
    const location = args.location || '';
    const requestedTime = args.time || null;
    
    if (!location) {
      return JSON.stringify({ error: 'Location parameter is required', example: '{"location":"San Francisco, CA"}' });
    }
    
    // Check if location is lat,lon format
    let lat: number, lon: number;
    const latLonMatch = location.match(/^([\-\d.]+)\s*,\s*([\-\d.]+)$/);
    
    if (latLonMatch) {
      lat = parseFloat(latLonMatch[1]);
      lon = parseFloat(latLonMatch[2]);
    } else {
      // Geocode location using Open-Meteo's geocoding API
      const geocodeUrl = `https://geocoding-api.open-meteo.com/v1/search?name=${encodeURIComponent(location)}&count=1`;
      const geocodeResult = http_request('GET', geocodeUrl, {}, '');
      
      if (geocodeResult.status !== 200) {
        return JSON.stringify({ error: `Failed to geocode location: ${location}`, status: geocodeResult.status });
      }
      
      const geoData: GeoResult = JSON.parse(geocodeResult.body);
      
      if (!geoData.results || geoData.results.length === 0) {
        return JSON.stringify({ error: `Location not found: ${location}` });
      }
      
      lat = geoData.results[0].latitude;
      lon = geoData.results[0].longitude;
    }
    
    // Build weather API URL
    const hourlyParams = 'cloud_cover,relative_humidity_2m,wind_speed_10m';
    let weatherUrl = `https://api.open-meteo.com/v1/forecast?latitude=${lat}&longitude=${lon}&hourly=${hourlyParams}&timezone=auto`;
    
    if (requestedTime) {
      weatherUrl += `&time=${encodeURIComponent(requestedTime)}`;
    }
    
    const weatherResult = http_request('GET', weatherUrl, {}, '');
    
    if (weatherResult.status !== 200) {
      return JSON.stringify({ error: `Weather API error: ${weatherResult.status}`, details: weatherResult.body });
    }
    
    const weatherData: WeatherResponse = JSON.parse(weatherResult.body);
    
    // Get current or requested time index
    const now = new Date();
    let targetIndex = 0;
    
    if (weatherData.hourly.time && weatherData.hourly.time.length > 0) {
      // Find the closest hour
      for (let i = 0; i < weatherData.hourly.time.length; i++) {
        const hourTime = new Date(weatherData.hourly.time[i]);
        const diff = Math.abs(hourTime.getTime() - now.getTime());
        if (diff < 3600000) { // Within 1 hour
          targetIndex = i;
          break;
        }
      }
    }
    
    const cloudCover = weatherData.hourly.cloud_cover[targetIndex] || 0;
    const humidity = weatherData.hourly.relative_humidity_2m[targetIndex] || 0;
    const windSpeed = weatherData.hourly.wind_speed_10m[targetIndex] || 0;
    
    const skyQualityScore = calculateSkyQuality(cloudCover, humidity, windSpeed);
    const condition = formatCondition(skyQualityScore);
    
    const isClear = cloudCover < 20;
    
    const result = {
      location: {
        name: latLonMatch ? location : location,
        latitude: lat,
        longitude: lon
      },
      conditions: {
        cloud_cover: cloudCover,
        humidity: humidity,
        wind_speed: windSpeed
      },
      sky_quality: {
        score: skyQualityScore,
        label: condition.label,
        color: condition.color,
        recommendation: condition.recommendation
      },
      is_clear_sky: isClear,
      timestamp: new Date().toISOString()
    };
    
    return JSON.stringify(result, null, 2);
    
  } catch (error: any) {
    return JSON.stringify({ error: error.message || 'Unknown error occurred' });
  }
}

main();