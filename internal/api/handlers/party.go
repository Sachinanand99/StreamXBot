package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"streamgo/internal/config"
	"streamgo/internal/logger"
	"streamgo/internal/models"
	"streamgo/internal/services"
)

var logPartyHandler = logger.New("party_handler")

// PartyHandler exposes the StreamX listen-together wire-compatible HTTP and
// WebSocket API. It intentionally uses its own auth and error envelope rather
// than StreamXBot's JWT middleware and standard API response helpers.
type PartyHandler struct {
	partySvc *services.PartyService
	cfg      *config.Config
}

// NewPartyHandler creates a new PartyHandler.
func NewPartyHandler(partySvc *services.PartyService, cfg *config.Config) *PartyHandler {
	return &PartyHandler{partySvc: partySvc, cfg: cfg}
}

// Routes mounts all listen-together routes.
func (h *PartyHandler) Routes(r chi.Router) {
	r.Get("/healthz", h.Healthz)

	r.Get("/api/time", h.GetServerTime)
	r.Post("/api/parties", h.CreateParty)
	r.Post("/api/parties/{code}/join", h.JoinParty)
	r.Get("/api/parties/{code}", h.GetParty)
	r.Get("/api/parties/{code}/preview", h.PreviewParty)
	r.Post("/api/parties/{code}/leave", h.LeaveParty)

	r.Post("/api/presence", h.Presence)
	r.Get("/api/stats/live", h.LiveStats)
	r.Get("/api/stats/live/badge.svg", h.LiveBadge)

	r.Get("/invite/{code}", h.InviteLanding)
	r.Get("/ws/parties/{code}", h.WebSocketParty)
}

func writePartyJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writePartyError(w http.ResponseWriter, status int, code, message string) {
	writePartyJSON(w, status, map[string]string{
		"error":   code,
		"message": message,
	})
}

func parsePartyBearer(r *http.Request) string {
	parts := strings.SplitN(strings.TrimSpace(r.Header.Get("Authorization")), " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func (h *PartyHandler) decodePartyJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, h.partySvc.RequestMaxBytes())
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writePartyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body is too large.")
		} else {
			writePartyError(w, http.StatusUnprocessableEntity, "invalid_json", "Malformed JSON body.")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writePartyError(w, http.StatusUnprocessableEntity, "invalid_json", "Request body must contain one JSON object.")
		return false
	}
	return true
}

func (h *PartyHandler) clientIP(r *http.Request) string {
	return services.ClientIP(r, h.cfg != nil && h.cfg.PartyTrustProxy)
}

// Healthz is the upstream-compatible health probe used by the Android client.
func (h *PartyHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	writePartyJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"serverMs": h.partySvc.NowMs(),
	})
}

// GetServerTime returns the monotonic server clock used for playback sync.
func (h *PartyHandler) GetServerTime(w http.ResponseWriter, r *http.Request) {
	writePartyJSON(w, http.StatusOK, map[string]any{
		"serverMs": h.partySvc.NowMs(),
	})
}

// CreateParty creates a new party and joins the creator as host.
func (h *PartyHandler) CreateParty(w http.ResponseWriter, r *http.Request) {
	var req models.PartyJoinRequest
	if !h.decodePartyJSONBody(w, r, &req) {
		return
	}
	resp, perr := h.partySvc.CreateParty(h.clientIP(r), &req)
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusCreated, resp)
}

// JoinParty joins an existing party.
func (h *PartyHandler) JoinParty(w http.ResponseWriter, r *http.Request) {
	var req models.PartyJoinRequest
	if !h.decodePartyJSONBody(w, r, &req) {
		return
	}
	resp, perr := h.partySvc.JoinParty(chi.URLParam(r, "code"), &req)
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusOK, resp)
}

// GetParty returns a full authenticated party snapshot.
func (h *PartyHandler) GetParty(w http.ResponseWriter, r *http.Request) {
	token := parsePartyBearer(r)
	if token == "" {
		writePartyError(w, http.StatusUnauthorized, "unauthorized", "Missing bearer token.")
		return
	}
	partyWire, perr := h.partySvc.GetParty(chi.URLParam(r, "code"), token)
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusOK, partyWire)
}

// PreviewParty returns public membership information without requiring a token.
func (h *PartyHandler) PreviewParty(w http.ResponseWriter, r *http.Request) {
	preview, perr := h.partySvc.PreviewParty(chi.URLParam(r, "code"))
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusOK, preview)
}

// LeaveParty removes the authenticated member from the party.
func (h *PartyHandler) LeaveParty(w http.ResponseWriter, r *http.Request) {
	token := parsePartyBearer(r)
	if token == "" {
		writePartyError(w, http.StatusUnauthorized, "unauthorized", "Missing bearer token.")
		return
	}
	resp, perr := h.partySvc.LeaveParty(chi.URLParam(r, "code"), token)
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusOK, resp)
}

// Presence records an open-app heartbeat from a native client.
func (h *PartyHandler) Presence(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		writePartyError(w, http.StatusForbidden, "origin_not_allowed", "Presence is only accepted from the apps.")
		return
	}
	var req models.PresenceRequest
	if !h.decodePartyJSONBody(w, r, &req) {
		return
	}
	resp, perr := h.partySvc.HandlePresence(h.clientIP(r), &req)
	if perr != nil {
		writePartyError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writePartyJSON(w, http.StatusOK, resp)
}

// LiveStats returns current open-app counts.
func (h *PartyHandler) LiveStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=15")
	writePartyJSON(w, http.StatusOK, h.partySvc.LiveStats())
}

// LiveBadge renders the shields-style listening-now badge.
func (h *PartyHandler) LiveBadge(w http.ResponseWriter, r *http.Request) {
	online := 0
	if stats, ok := h.partySvc.LiveStats().(models.PartyLiveStats); ok {
		online = stats.Online
	}
	value := fmt.Sprint(online)
	valueWidth := 8.25*float64(len(value)) + 24
	width := 124.25 + valueWidth
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%g" height="28" role="img" aria-label="LISTENING NOW: %s"><title>LISTENING NOW: %s</title><g shape-rendering="crispEdges"><rect width="124.25" height="28" fill="#0d1117"/><rect x="124.25" width="%g" height="28" fill="#fb4f67"/></g><g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="100"><text transform="scale(.1)" x="621.25" y="175" textLength="1002.5">LISTENING NOW</text><text transform="scale(.1)" x="%g" y="175" textLength="%g" font-weight="bold">%s</text></g></svg>`,
		width, value, value, valueWidth, (124.25+valueWidth/2)*10, 82.5*float64(len(value)), value)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "image/svg+xml;charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

func (h *PartyHandler) requestOrigin(r *http.Request) string {
	if h.cfg != nil && h.cfg.PartyPublicOrigin != "" {
		return h.cfg.PartyPublicOrigin
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	if h.cfg != nil && h.cfg.PartyTrustProxy {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded != "" {
			proto = forwarded
		}
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%s://%s", proto, host)
}

func (h *PartyHandler) browserOriginAllowed(origin string) bool {
	if h.cfg == nil {
		return false
	}
	for _, allowed := range h.cfg.PartyAllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

type partyInvitePageData struct {
	Code              string
	DeepLink          string
	IntentURI         template.URL
	SafeDeepLink      template.URL
	ServerOrigin      string
	CurrentSongTitle  string
	CurrentSongArtist string
	MemberCount       int
	IsActive          bool
}

var partyInviteTemplate = template.Must(template.New("party_invite").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>StreamX Listen Together - Party {{.Code}}</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            background-color: #0b0b0e;
            color: #f3f3f7;
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
            padding: 24px;
        }
        .container {
            background: rgba(22, 22, 30, 0.85);
            backdrop-filter: blur(24px);
            -webkit-backdrop-filter: blur(24px);
            border: 1px solid rgba(255, 255, 255, 0.08);
            border-radius: 28px;
            max-width: 440px;
            width: 100%;
            padding: 36px 28px;
            text-align: center;
            box-shadow: 0 20px 50px rgba(0, 0, 0, 0.5), 0 0 40px rgba(124, 77, 255, 0.1);
        }
        .badge {
            display: inline-flex;
            align-items: center;
            gap: 6px;
            background: rgba(124, 77, 255, 0.15);
            color: #b388ff;
            font-size: 13px;
            font-weight: 600;
            padding: 6px 14px;
            border-radius: 9999px;
            margin-bottom: 20px;
            letter-spacing: 0.5px;
        }
        .badge-dot {
            width: 8px;
            height: 8px;
            background: #00e676;
            border-radius: 50%;
            box-shadow: 0 0 8px #00e676;
        }
        .badge-dot.offline {
            background: #ff5252;
            box-shadow: 0 0 8px #ff5252;
        }
        h1 {
            font-size: 24px;
            font-weight: 700;
            margin-bottom: 8px;
            letter-spacing: -0.5px;
        }
        .subtitle {
            color: #9e9ea7;
            font-size: 15px;
            line-height: 1.5;
            margin-bottom: 28px;
        }
        .code-box {
            background: rgba(255, 255, 255, 0.04);
            border: 1px solid rgba(255, 255, 255, 0.1);
            border-radius: 18px;
            padding: 16px 20px;
            margin-bottom: 28px;
        }
        .code-label {
            font-size: 12px;
            color: #71717a;
            text-transform: uppercase;
            letter-spacing: 1.5px;
            margin-bottom: 6px;
        }
        .code-val {
            font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
            font-size: 32px;
            font-weight: 800;
            letter-spacing: 6px;
            color: #ffffff;
        }
        .now-playing {
            font-size: 14px;
            color: #d1d1d6;
            margin-top: 10px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .btn {
            display: block;
            width: 100%;
            padding: 16px 24px;
            background: linear-gradient(135deg, #7c4dff 0%, #3d5afe 100%);
            color: #ffffff;
            font-size: 16px;
            font-weight: 600;
            text-decoration: none;
            border-radius: 16px;
            border: none;
            cursor: pointer;
            transition: transform 0.15s ease, opacity 0.15s ease;
            box-shadow: 0 8px 24px rgba(124, 77, 255, 0.35);
        }
        .btn:hover {
            transform: translateY(-1px);
            opacity: 0.95;
        }
        .btn:active {
            transform: scale(0.98);
        }
        .footer-note {
            margin-top: 24px;
            font-size: 13px;
            color: #71717a;
            line-height: 1.5;
        }
        .footer-note a {
            color: #b388ff;
            text-decoration: none;
        }
        .footer-note a:hover {
            text-decoration: underline;
        }
    </style>
</head>
<body>
    <div class="container">
        {{if .IsActive}}
            <div class="badge">
                <span class="badge-dot"></span>
                <span>Listen Together • {{.MemberCount}} in party</span>
            </div>
            <h1>Join the Music Party</h1>
            <p class="subtitle">Opening StreamX to sync playback in real time.</p>
            <div class="code-box">
                <div class="code-label">Party Code</div>
                <div class="code-val">{{.Code}}</div>
                {{if .CurrentSongTitle}}
                    <div class="now-playing">🎵 {{.CurrentSongTitle}} - {{.CurrentSongArtist}}</div>
                {{end}}
            </div>
            <a id="joinBtn" href="{{.IntentURI}}" class="btn">Join Party in StreamX</a>
            <p class="footer-note">
                Didn’t open automatically? Tap the button above.<br>
                Don't have StreamX yet? <a href="https://github.com/StreamXProject/StreamX/releases" target="_blank" rel="noopener">Download it here</a>.
            </p>
            <script>
                var intentUri = {{.IntentURI}};
                var deepLink = {{.SafeDeepLink}};
                function launch() {
                    if (/Android/i.test(navigator.userAgent)) {
                        window.location.href = intentUri;
                    } else {
                        window.location.href = deepLink;
                    }
                }
                setTimeout(launch, 100);
            </script>
        {{else}}
            <div class="badge">
                <span class="badge-dot offline"></span>
                <span>Party Inactive</span>
            </div>
            <h1>Party Not Found</h1>
            <p class="subtitle">This party code has expired or does not exist on this server.</p>
            <div class="code-box">
                <div class="code-label">Party Code</div>
                <div class="code-val" style="color: #a1a1aa;">{{.Code}}</div>
            </div>
            <p class="footer-note">
                Please ask the host for a new invite link or check your server configuration.
            </p>
        {{end}}
    </div>
</body>
</html>`))

// InviteLanding serves the web invite/deep-link page for a party code.
func (h *PartyHandler) InviteLanding(w http.ResponseWriter, r *http.Request) {
	rawCode := chi.URLParam(r, "code")
	code := services.NormalisePartyCode(rawCode)
	origin := h.requestOrigin(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if len(code) != 6 {
		w.WriteHeader(http.StatusBadRequest)
		_ = partyInviteTemplate.Execute(w, partyInvitePageData{
			Code:     html.EscapeString(rawCode),
			IsActive: false,
		})
		return
	}

	info := h.partySvc.InviteInfo(code)
	if !info.Active {
		w.WriteHeader(http.StatusNotFound)
		_ = partyInviteTemplate.Execute(w, partyInvitePageData{
			Code:     code,
			IsActive: false,
		})
		return
	}

	deepLink := fmt.Sprintf("streamx://party/%s?server=%s", url.PathEscape(code), url.QueryEscape(origin))
	intentURI := fmt.Sprintf("intent://party/%s?server=%s#Intent;scheme=streamx;end", url.PathEscape(code), url.QueryEscape(origin))
	_ = partyInviteTemplate.Execute(w, partyInvitePageData{
		Code:              code,
		DeepLink:          deepLink,
		IntentURI:         template.URL(intentURI),
		SafeDeepLink:      template.URL(deepLink),
		ServerOrigin:      origin,
		CurrentSongTitle:  info.Title,
		CurrentSongArtist: info.Artist,
		MemberCount:       info.MemberCount,
		IsActive:          true,
	})
}

// WebSocketParty upgrades a party socket after validating the party bearer token.
func (h *PartyHandler) WebSocketParty(w http.ResponseWriter, r *http.Request) {
	token := parsePartyBearer(r)
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !h.browserOriginAllowed(origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	partyState, member, perr := h.partySvc.AuthenticateSocket(chi.URLParam(r, "code"), token)
	if perr != nil {
		http.Error(w, perr.Message, perr.Status)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin is explicitly checked above.
	})
	if err != nil {
		logPartyHandler.Warnf("party websocket upgrade failed: %v", err)
		return
	}
	h.partySvc.ServeWebSocket(r.Context(), partyState, member, conn)
}
