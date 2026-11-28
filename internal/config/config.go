package config

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"streamgo/internal/logger"
)

var log = logger.New("config")

// Config holds all configuration parameters for StreamGO.
type Config struct {
	Port                     string
	Debug                    bool
	APILogs                  bool
	CorsOrigin               string
	MongoURI                 string
	DatabaseName             string
	ApiID                    int
	ApiHash                  string
	BotToken                 string
	SecretKey                string
	SessionString            string
	ChannelID                int64
	DumpChannelID            int64
	MultiClients             bool
	MultiClientTokens        []string
	FilterMode               int
	ChatTopic                string
	CollaboratorIDs          []int64
	Lyrics                   bool
	LRCLIB                   bool
	Musixmatch               bool
	TelegramOIDCClientID     string
	TelegramOIDCClientSecret string
	TelegramOIDCOrigin       string
	TelegramOIDCRedirectURI  string
	CookieSecure             bool
	CookieSameSite           string
	OwnerIDs                 []int64
	SudoUsers                []int64
	EnrichmentWorkers        int
	GuestPassword            string
	AlacCacheMaxBytes        int64
	AlacCacheMaxFiles        int

	// Listen Together / party server
	PartyMaxMembers              int
	PartyStateHeartbeatMS        int
	PartyPlayLeadMS              int
	PartyDisconnectGraceMS       int64
	PartyEmptyTTLMS              int64
	PartyMaxAgeMS                int64
	PartyControlRatePerSecond    int
	PartyFrameRatePerSecond      int
	PartyMaxUpcomingQueue        int
	PartyMaxQueueLength          int
	PartyMaxParties              int
	PartyCreateRatePerMinute     int
	PartyRateLimitMaxEntries     int
	PartyRequestMaxBytes         int64
	PartyWebSocketMaxBytes       int64
	PartyConnectionIdleMS        int64
	PartyAllowedOrigins          []string
	PartyTrustProxy              bool
	PartyPublicOrigin            string
	PartyPresenceIntervalSec     int
	PartyPresenceGraceSec        int
	PartyPresenceMaxEntries      int
	PartyPresencePingsPerMinute  int
	PartyPresenceRateLimitMaxIPs int

	// Cloudflare R2 / S3 Blob Storage (Optional)
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2BucketName      string
	R2PublicURL       string
}

// Load reads configuration from .env file (if present) and environment variables.
func Load() *Config {
	// Try loading .env; ignore error if not found (e.g., in container/production)
	if err := godotenv.Load(); err != nil {
		log.Info("No .env file found, using system environment variables")
	} else {
		log.Info("Loaded configuration from .env")
	}

	debug := getEnvBool("DEBUG", false)
	logger.SetDebug(debug)

	mongoURI := strings.TrimSpace(getEnv("MONGO_URI", "mongodb://localhost:27017"))
	// Sanitize any accidental leading equals sign (e.g. MONGO_URI==...)
	mongoURI = strings.TrimPrefix(mongoURI, "=")

	multiClients := getEnvBool("MULTI_CLIENTS", false)
	var multiTokens []string
	for _, key := range []string{"MULTI_CLIENTS_1", "MULTI_CLIENTS_2", "MULTI_CLIENTS_3", "MULTI_CLIENTS_4"} {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			multiTokens = append(multiTokens, val)
		}
	}
	if extra := strings.TrimSpace(os.Getenv("MULTI_CLIENT_TOKENS")); extra != "" {
		for _, part := range strings.Split(extra, ",") {
			if p := strings.TrimSpace(part); p != "" {
				multiTokens = append(multiTokens, p)
			}
		}
	}
	if len(multiTokens) > 0 {
		multiClients = true
	}

	// Parse collaborator IDs from COLLABORATOR_ID and COLLABORATOR_IDS
	collabList := parseIDList(os.Getenv("COLLABORATOR_IDS"))
	if singleCollab := getEnvInt64("COLLABORATOR_ID", 0); singleCollab != 0 {
		collabList = append(collabList, singleCollab)
	}

	// Owner and Sudo users
	ownerIDs := parseIDList(os.Getenv("OWNER_ID"))
	if len(ownerIDs) == 0 {
		if oid := getEnvInt64("OWNER_ID", 0); oid != 0 {
			ownerIDs = []int64{oid}
		}
	}
	sudoUsers := parseIDList(os.Getenv("SUDO_USERS"))

	partyMaxUpcomingQueue := getEnvInt("JAM_MAX_UPCOMING_QUEUE", 25)
	partyMaxQueueLength := getEnvInt("JAM_MAX_QUEUE_LENGTH", 1+partyMaxUpcomingQueue)
	partyAllowedOrigins := parseStringList(getEnv("JAM_ALLOWED_ORIGINS", ""))

	return &Config{
		Port:                     getEnv("PORT", "8000"),
		Debug:                    debug,
		APILogs:                  getEnvBool("API_LOGS", getEnvBool("HTTP_LOGS", false)),
		CorsOrigin:               getEnv("CORS_ORIGIN", "*"),
		MongoURI:                 mongoURI,
		DatabaseName:             getEnv("DATABASE_NAME", "Stream"),
		ApiID:                    getEnvInt("API_ID", 0),
		ApiHash:                  strings.TrimSpace(os.Getenv("API_HASH")),
		BotToken:                 strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		SecretKey:                strings.TrimSpace(os.Getenv("SECRET_KEY")),
		SessionString:            strings.TrimSpace(os.Getenv("SESSION_STRING")),
		ChannelID:                getEnvInt64("CHANNEL_ID", 0),
		DumpChannelID:            getEnvInt64("DUMP_CHANNEL_ID", 0),
		MultiClients:             multiClients,
		MultiClientTokens:        multiTokens,
		FilterMode:               getEnvInt("FILTER_MODE", 0),
		ChatTopic:                strings.TrimSpace(getEnv("CHAT_TOPIC", "all")),
		CollaboratorIDs:          collabList,
		Lyrics:                   getEnvBool("LYRICS", true),
		LRCLIB:                   getEnvBool("LRCLIB", false),
		Musixmatch:               getEnvBool("MUSIXMATCH", true),
		TelegramOIDCClientID:     strings.TrimSpace(os.Getenv("TELEGRAM_OIDC_CLIENT_ID")),
		TelegramOIDCClientSecret: strings.TrimSpace(os.Getenv("TELEGRAM_OIDC_CLIENT_SECRET")),
		TelegramOIDCOrigin:       strings.TrimSpace(os.Getenv("TELEGRAM_OIDC_ORIGIN")),
		TelegramOIDCRedirectURI:  strings.TrimSpace(os.Getenv("TELEGRAM_OIDC_REDIRECT_URI")),
		CookieSecure:             getEnvBool("COOKIE_SECURE", false),
		CookieSameSite:           getEnv("COOKIE_SAMESITE", "lax"),
		OwnerIDs:                 ownerIDs,
		SudoUsers:                sudoUsers,
		EnrichmentWorkers:        getEnvInt("ENRICHMENT_WORKERS", 2),
		GuestPassword:            getEnv("GUEST_PASSWORD", ""),
		AlacCacheMaxBytes:        getEnvInt64("ALAC_CACHE_MAX_BYTES", 5*1024*1024*1024),
		AlacCacheMaxFiles:        getEnvInt("ALAC_CACHE_MAX_FILES", 200),

		PartyMaxMembers:              getEnvInt("JAM_MAX_MEMBERS", 5),
		PartyStateHeartbeatMS:        getEnvInt("JAM_STATE_HEARTBEAT_MS", 5000),
		PartyPlayLeadMS:              getEnvInt("JAM_PLAY_LEAD_MS", 350),
		PartyDisconnectGraceMS:       getEnvInt64("JAM_DISCONNECT_GRACE_MS", 45000),
		PartyEmptyTTLMS:              getEnvInt64("JAM_EMPTY_PARTY_TTL_MS", 120000),
		PartyMaxAgeMS:                getEnvInt64("JAM_PARTY_MAX_AGE_MS", 12*60*60*1000),
		PartyControlRatePerSecond:    getEnvInt("JAM_CONTROL_RATE_PER_SECOND", 25),
		PartyFrameRatePerSecond:      getEnvInt("JAM_FRAME_RATE_PER_SECOND", 30),
		PartyMaxUpcomingQueue:        partyMaxUpcomingQueue,
		PartyMaxQueueLength:          partyMaxQueueLength,
		PartyMaxParties:              getEnvInt("JAM_MAX_PARTIES", 50),
		PartyCreateRatePerMinute:     getEnvInt("JAM_CREATE_RATE_PER_MINUTE", 2),
		PartyRateLimitMaxEntries:     getEnvInt("JAM_RATE_LIMIT_MAX_ENTRIES", 10000),
		PartyRequestMaxBytes:         getEnvInt64("JAM_REQUEST_MAX_BYTES", 16*1024),
		PartyWebSocketMaxBytes:       getEnvInt64("JAM_WEBSOCKET_MAX_BYTES", 16*1024),
		PartyConnectionIdleMS:        getEnvInt64("JAM_CONNECTION_IDLE_MS", 15*60*1000),
		PartyAllowedOrigins:          partyAllowedOrigins,
		PartyTrustProxy:              getEnvBool("JAM_TRUST_PROXY", false),
		PartyPublicOrigin:            getPartyOrigin(getEnv("JAM_PUBLIC_ORIGIN", "")),
		PartyPresenceIntervalSec:     getEnvInt("JAM_PRESENCE_INTERVAL_SEC", 300),
		PartyPresenceGraceSec:        getEnvInt("JAM_PRESENCE_GRACE_SEC", 90),
		PartyPresenceMaxEntries:      getEnvInt("JAM_PRESENCE_MAX_ENTRIES", 200000),
		PartyPresencePingsPerMinute:  getEnvInt("JAM_PRESENCE_PINGS_PER_MINUTE", 20),
		PartyPresenceRateLimitMaxIPs: getEnvInt("JAM_PRESENCE_RATE_LIMIT_MAX_IPS", 100000),

		R2AccountID:       getEnv("R2_ACCOUNT_ID", ""),
		R2AccessKeyID:     getEnv("R2_ACCESS_KEY_ID", ""),
		R2SecretAccessKey: getEnv("R2_SECRET_ACCESS_KEY", ""),
		R2BucketName:      getEnv("R2_BUCKET_NAME", ""),
		R2PublicURL:       strings.TrimRight(getEnv("R2_PUBLIC_URL", ""), "/"),
	}
}

// HasR2 returns true if all necessary Cloudflare R2 credentials are provided.
func (c *Config) HasR2() bool {
	return c.R2AccountID != "" && c.R2AccessKeyID != "" && c.R2SecretAccessKey != "" && c.R2BucketName != ""
}

func parseStringList(raw string) []string {
	var values []string
	for _, item := range strings.Split(raw, ",") {
		if value := strings.TrimSpace(item); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func getPartyOrigin(raw string) string {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		log.Warnf("ignoring invalid JAM_PUBLIC_ORIGIN %q", value)
		return ""
	}
	return value
}

func parseIDList(raw string) []int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// Try JSON array first
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var arr []any
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			var ids []int64
			for _, item := range arr {
				switch v := item.(type) {
				case float64:
					ids = append(ids, int64(v))
				case string:
					if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
						ids = append(ids, n)
					}
				}
			}
			return ids
		}
	}

	// Delimiter split (comma, space)
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
	var ids []int64
	for _, f := range fields {
		if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
		v := strings.TrimSpace(val)
		v = strings.Trim(v, "\"'")
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		val = strings.ToLower(strings.TrimSpace(val))
		return val == "true" || val == "1" || val == "yes"
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return n
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			return n
		}
	}
	return defaultVal
}
