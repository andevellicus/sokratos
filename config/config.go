package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"sokratos/logger"
)

// AppConfig holds all parsed environment configuration.
type AppConfig struct {
	TelegramToken string
	AllowedIDs    map[int64]struct{}

	LLMURL   string
	LLMModel string

	SearxngURL string
	RsshubURL  string

	EmbedURL   string
	EmbedModel string

	DeepThinkerURL   string
	DeepThinkerModel string

	SubagentURL   string
	SubagentModel string
	SubagentSlots int

	BrainURL   string // when set, enables two-model mode (Brain + Subagent)
	BrainModel string

	MaxWebSources     int
	MemorySearchLimit int
	MaxToolResultLen  int

	ConsolidationMemoryLimit int
	HeartbeatInterval        time.Duration
	RoutineInterval          time.Duration
	RoutineTimeout           time.Duration

	CognitiveBufferThreshold int
	LullDuration             time.Duration
	CognitiveCeiling         time.Duration

	MemoryStalenessDays       int
	WorkItemsTTLDays          int
	ProcessedEmailsTTLDays    int
	ProcessedEventsTTLDays    int
	FailedOpsTTLDays          int
	SkillKVTTLDays            int
	ReflectionMemoryThreshold int

	MaintenanceInterval   time.Duration
	DBMaxConns            int
	DBMinConns            int
	DBMaxConnLifetime     time.Duration
	DBMaxConnIdleTime     time.Duration
	DBHealthCheckPeriod   time.Duration
	ConfirmationTimeout   time.Duration
	MaxSupersededProfiles int
	EmailDisplayBatch     int

	DatabaseURL string

	GmailCredsPath  string
	GoogleTokenPath string

	AgentName string
}

// Load parses all environment variables into an AppConfig struct.
func Load() *AppConfig {
	return &AppConfig{
		TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		AllowedIDs:    parseAllowedIDs(os.Getenv("ALLOWED_TELEGRAM_IDS")),

		LLMURL:   EnvString("LLM_URL", "http://localhost:11434"),
		LLMModel: os.Getenv("LLM_MODEL"),

		SearxngURL: os.Getenv("SEARXNG_URL"),
		RsshubURL:  os.Getenv("RSSHUB_URL"),

		EmbedURL:   os.Getenv("EMBEDDING_URL"),
		EmbedModel: os.Getenv("EMBEDDING_MODEL"),

		DeepThinkerURL:   os.Getenv("DEEP_THINKER_URL"),
		DeepThinkerModel: os.Getenv("DEEP_THINKER_MODEL"),

		SubagentURL:   os.Getenv("SUBAGENT_URL"),
		SubagentModel: os.Getenv("SUBAGENT_MODEL"),
		SubagentSlots: EnvInt("SUBAGENT_SLOTS", 2),

		BrainURL:   os.Getenv("BRAIN_URL"),
		BrainModel: os.Getenv("BRAIN_MODEL"),

		MaxWebSources:     EnvInt("MAX_WEB_SOURCES", 2),
		MemorySearchLimit: EnvInt("MEMORY_SEARCH_LIMIT", 10),
		MaxToolResultLen:  EnvInt("MAX_TOOL_RESULT_LEN", 8000),

		ConsolidationMemoryLimit: EnvInt("CONSOLIDATION_MEMORY_LIMIT", 50),
		HeartbeatInterval:        EnvDuration("HEARTBEAT_INTERVAL", 5*time.Minute),
		RoutineInterval:          EnvDuration("ROUTINE_INTERVAL", 30*time.Second),
		RoutineTimeout:           EnvDuration("ROUTINE_TIMEOUT", 5*time.Minute),

		CognitiveBufferThreshold: EnvInt("COGNITIVE_BUFFER_THRESHOLD", 20),
		LullDuration:             EnvDuration("LULL_DURATION", 20*time.Minute),
		CognitiveCeiling:         EnvDuration("COGNITIVE_CEILING", 4*time.Hour),

		MemoryStalenessDays:       EnvInt("MEMORY_STALENESS_DAYS", 90),
		WorkItemsTTLDays:          EnvInt("WORK_ITEMS_TTL_DAYS", 7),
		ProcessedEmailsTTLDays:    EnvInt("PROCESSED_EMAILS_TTL_DAYS", 90),
		ProcessedEventsTTLDays:    EnvInt("PROCESSED_EVENTS_TTL_DAYS", 90),
		FailedOpsTTLDays:          EnvInt("FAILED_OPS_TTL_DAYS", 30),
		SkillKVTTLDays:            EnvInt("SKILL_KV_TTL_DAYS", 90),
		ReflectionMemoryThreshold: EnvInt("REFLECTION_MEMORY_THRESHOLD", 50),

		MaintenanceInterval:   EnvDuration("MAINTENANCE_INTERVAL", 30*time.Minute),
		DBMaxConns:            EnvInt("DB_MAX_CONNS", 20),
		DBMinConns:            EnvInt("DB_MIN_CONNS", 2),
		DBMaxConnLifetime:     EnvDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute),
		DBMaxConnIdleTime:     EnvDuration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute),
		DBHealthCheckPeriod:   EnvDuration("DB_HEALTH_CHECK_PERIOD", 30*time.Second),
		ConfirmationTimeout:   EnvDuration("CONFIRMATION_TIMEOUT", 2*time.Minute),
		MaxSupersededProfiles: EnvInt("MAX_SUPERSEDED_PROFILES", 20),
		EmailDisplayBatch:     EnvInt("EMAIL_DISPLAY_BATCH", 5),

		DatabaseURL: os.Getenv("DATABASE_URL"),

		GmailCredsPath:  EnvString("GMAIL_CREDENTIALS_PATH", ".credentials/credentials.json"),
		GoogleTokenPath: EnvString("GOOGLE_TOKEN_PATH", ".credentials/token.json"),

		AgentName: EnvString("AGENT_NAME", "Sokratos"),
	}
}

// EnvString returns the value of the environment variable named by key, or def
// if the variable is not set or empty.
func EnvString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvInt returns the integer value of the environment variable named by key,
// or def if the variable is not set, empty, or cannot be parsed.
func EnvInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.Log.Warnf("Invalid %s %q, using default %d", key, raw, def)
		return def
	}
	return n
}

// EnvFloat returns the float64 value of the environment variable named by key,
// or def if the variable is not set, empty, or cannot be parsed.
func EnvFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		logger.Log.Warnf("Invalid %s %q, using default %.1f", key, raw, def)
		return def
	}
	return v
}

// EnvDuration returns the time.Duration value of the environment variable
// named by key, or def if the variable is not set, empty, or cannot be parsed.
func EnvDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Log.Warnf("Invalid %s %q, using default %s", key, raw, def)
		return def
	}
	return d
}

// parseAllowedIDs parses a comma-separated list of Telegram user IDs.
func parseAllowedIDs(raw string) map[int64]struct{} {
	allowed := make(map[int64]struct{})
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		allowed[id] = struct{}{}
	}
	return allowed
}
