package services

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"streamgo/internal/config"
	"streamgo/internal/logger"
	"streamgo/internal/models"
)

var partyLog = logger.New("party_service")

const (
	partyCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTUVWXYZ"
	partyCodeLength   = 6

	frameWelcome  = "welcome"
	frameState    = "state"
	frameQueue    = "queue"
	frameMembers  = "members"
	framePong     = "pong"
	frameError    = "error"
	frameBye      = "bye"
	frameActivity = "activity"

	framePing      = "ping"
	frameControl   = "control"
	frameSync      = "sync"
	frameSyncQueue = "syncQueue"
	frameReport    = "report"

	actionPlay        = "play"
	actionPause       = "pause"
	actionSeek        = "seek"
	actionSetTrack    = "setTrack"
	actionSetQueue    = "setQueue"
	actionQueueAdd    = "queueAdd"
	actionQueueRemove = "queueRemove"
	actionQueueClear  = "queueClear"
	actionQueueMove   = "queueMove"
	actionNext        = "next"
	actionPrevious    = "previous"
	actionKick        = "kick"
	actionSetMax      = "setMaxMembers"
	actionSetAutoplay = "setAutoplay"
	actionSetHostOnly = "setHostOnlyControl"
)

var partyControlActions = map[string]bool{
	actionPlay:        true,
	actionPause:       true,
	actionSeek:        true,
	actionSetTrack:    true,
	actionSetQueue:    true,
	actionQueueAdd:    true,
	actionQueueRemove: true,
	actionQueueClear:  true,
	actionQueueMove:   true,
	actionNext:        true,
	actionPrevious:    true,
	actionSetAutoplay: true,
}

// PartyService owns all in-memory party state for the listen-together feature.
type PartyService struct {
	cfg             *config.Config
	clock           *partyClock
	store           *partyStore
	hub             *partyHub
	presence        *partyPresenceTracker
	createLimiter   *partyIPRateLimiter
	presenceLimiter *partyIPRateLimiter
}

// NewPartyService creates the party service with upstream-compatible defaults.
func NewPartyService(cfg *config.Config) *PartyService {
	c := partyConfigOrDefault(cfg)
	s := &PartyService{
		cfg:   c,
		clock: newPartyClock(),
	}
	s.store = newPartyStore()
	s.hub = newPartyHub()
	s.presence = newPartyPresenceTracker(
		time.Duration(c.PartyPresenceIntervalSec+c.PartyPresenceGraceSec)*time.Second,
		c.PartyPresenceMaxEntries,
	)
	s.createLimiter = newPartyIPRateLimiter(
		time.Minute,
		c.PartyCreateRatePerMinute,
		c.PartyRateLimitMaxEntries,
	)
	s.presenceLimiter = newPartyIPRateLimiter(
		time.Minute,
		c.PartyPresencePingsPerMinute,
		c.PartyPresenceRateLimitMaxIPs,
	)
	return s
}

func partyConfigOrDefault(cfg *config.Config) *config.Config {
	defaults := &config.Config{
		PartyMaxMembers:              5,
		PartyStateHeartbeatMS:        5000,
		PartyPlayLeadMS:              350,
		PartyDisconnectGraceMS:       45000,
		PartyEmptyTTLMS:              120000,
		PartyMaxAgeMS:                12 * 60 * 60 * 1000,
		PartyControlRatePerSecond:    25,
		PartyFrameRatePerSecond:      30,
		PartyMaxUpcomingQueue:        25,
		PartyMaxQueueLength:          26,
		PartyMaxParties:              50,
		PartyCreateRatePerMinute:     2,
		PartyRateLimitMaxEntries:     10000,
		PartyRequestMaxBytes:         16 * 1024,
		PartyWebSocketMaxBytes:       16 * 1024,
		PartyConnectionIdleMS:        15 * 60 * 1000,
		PartyPresenceIntervalSec:     300,
		PartyPresenceGraceSec:        90,
		PartyPresenceMaxEntries:      200000,
		PartyPresencePingsPerMinute:  20,
		PartyPresenceRateLimitMaxIPs: 100000,
	}
	if cfg == nil {
		return defaults
	}
	c := *cfg
	if c.PartyMaxMembers <= 0 {
		c.PartyMaxMembers = defaults.PartyMaxMembers
	}
	if c.PartyStateHeartbeatMS <= 0 {
		c.PartyStateHeartbeatMS = defaults.PartyStateHeartbeatMS
	}
	if c.PartyPlayLeadMS < 0 {
		c.PartyPlayLeadMS = defaults.PartyPlayLeadMS
	}
	if c.PartyDisconnectGraceMS <= 0 {
		c.PartyDisconnectGraceMS = defaults.PartyDisconnectGraceMS
	}
	if c.PartyEmptyTTLMS <= 0 {
		c.PartyEmptyTTLMS = defaults.PartyEmptyTTLMS
	}
	if c.PartyMaxAgeMS <= 0 {
		c.PartyMaxAgeMS = defaults.PartyMaxAgeMS
	}
	if c.PartyControlRatePerSecond <= 0 {
		c.PartyControlRatePerSecond = defaults.PartyControlRatePerSecond
	}
	if c.PartyFrameRatePerSecond <= 0 {
		c.PartyFrameRatePerSecond = defaults.PartyFrameRatePerSecond
	}
	if c.PartyMaxUpcomingQueue <= 0 {
		c.PartyMaxUpcomingQueue = defaults.PartyMaxUpcomingQueue
	}
	if c.PartyMaxQueueLength <= c.PartyMaxUpcomingQueue {
		c.PartyMaxQueueLength = c.PartyMaxUpcomingQueue + 1
	}
	if c.PartyMaxParties <= 0 {
		c.PartyMaxParties = defaults.PartyMaxParties
	}
	if c.PartyCreateRatePerMinute <= 0 {
		c.PartyCreateRatePerMinute = defaults.PartyCreateRatePerMinute
	}
	if c.PartyRateLimitMaxEntries <= 0 {
		c.PartyRateLimitMaxEntries = defaults.PartyRateLimitMaxEntries
	}
	if c.PartyRequestMaxBytes <= 0 {
		c.PartyRequestMaxBytes = defaults.PartyRequestMaxBytes
	}
	if c.PartyWebSocketMaxBytes <= 0 {
		c.PartyWebSocketMaxBytes = defaults.PartyWebSocketMaxBytes
	}
	if c.PartyConnectionIdleMS <= 0 {
		c.PartyConnectionIdleMS = defaults.PartyConnectionIdleMS
	}
	if c.PartyPresenceIntervalSec <= 0 {
		c.PartyPresenceIntervalSec = defaults.PartyPresenceIntervalSec
	}
	if c.PartyPresenceGraceSec <= 0 {
		c.PartyPresenceGraceSec = defaults.PartyPresenceGraceSec
	}
	if c.PartyPresenceMaxEntries <= 0 {
		c.PartyPresenceMaxEntries = defaults.PartyPresenceMaxEntries
	}
	if c.PartyPresencePingsPerMinute <= 0 {
		c.PartyPresencePingsPerMinute = defaults.PartyPresencePingsPerMinute
	}
	if c.PartyPresenceRateLimitMaxIPs <= 0 {
		c.PartyPresenceRateLimitMaxIPs = defaults.PartyPresenceRateLimitMaxIPs
	}
	return &c
}

// partyError mirrors the upstream HTTP error payload.
type partyError struct {
	Status  int
	Code    string
	Message string
}

func (e *partyError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func newPartyError(status int, code, message string) *partyError {
	return &partyError{Status: status, Code: code, Message: message}
}

// partyClock advances monotonically so playback anchors never move backwards.
type partyClock struct {
	mu   sync.Mutex
	last int64
}

func newPartyClock() *partyClock {
	return &partyClock{last: time.Now().UnixMilli()}
}

func (c *partyClock) NowMs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixMilli()
	if now <= c.last {
		now = c.last + 1
	}
	c.last = now
	return now
}

func newPartyCode() string {
	alphaLen := big.NewInt(int64(len(partyCodeAlphabet)))
	var sb strings.Builder
	sb.Grow(partyCodeLength)
	for i := 0; i < partyCodeLength; i++ {
		idx, err := rand.Int(rand.Reader, alphaLen)
		if err != nil {
			sb.WriteByte(partyCodeAlphabet[0])
			continue
		}
		sb.WriteByte(partyCodeAlphabet[idx.Int64()])
	}
	return sb.String()
}

// NormalisePartyCode cleans a user-entered party code.
func NormalisePartyCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		switch r {
		case 'I', 'L':
			sb.WriteByte('1')
		case 'O':
			sb.WriteByte('0')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// IsValidPartyCode reports whether code is a canonical six-character party code.
func IsValidPartyCode(code string) bool {
	if len(code) != partyCodeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(partyCodeAlphabet, rune(code[i])) {
			return false
		}
	}
	return true
}

func validatePartyJoinRequest(req *models.PartyJoinRequest) error {
	if req == nil {
		return errors.New("identity is required")
	}
	req.UserID = strings.TrimSpace(req.UserID)
	if req.UserID == "" || len(req.UserID) > 128 {
		return errors.New("userId must be between 1 and 128 characters")
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	if req.DeviceID == "" || len(req.DeviceID) > 128 {
		return errors.New("deviceId must be between 1 and 128 characters")
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" || len(req.DisplayName) > 80 {
		return errors.New("displayName must be between 1 and 80 characters")
	}
	if req.AvatarURL != nil {
		value := strings.TrimSpace(*req.AvatarURL)
		if value == "" {
			req.AvatarURL = nil
		} else if len(value) > 1000 {
			return errors.New("avatarUrl must not exceed 1000 characters")
		} else if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return errors.New("avatarUrl must be an http(s) URL")
		} else {
			req.AvatarURL = &value
		}
	}
	if req.MaxMembers != nil && (*req.MaxMembers < 2 || *req.MaxMembers > 10) {
		return errors.New("maxMembers must be between 2 and 10")
	}
	return nil
}

func randomPartyToken(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func randomPartyHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// partyIPRateLimiter caps create and presence requests by client IP.
type partyIPRateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	maxKeys int
	entries map[string]partyRateEntry
	swept   time.Time
}

type partyRateEntry struct {
	started time.Time
	count   int
}

func newPartyIPRateLimiter(window time.Duration, limit, maxKeys int) *partyIPRateLimiter {
	return &partyIPRateLimiter{
		window:  window,
		limit:   limit,
		maxKeys: maxKeys,
		entries: make(map[string]partyRateEntry),
	}
}

func (l *partyIPRateLimiter) Allow(ip string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) >= time.Second || (l.maxKeys > 0 && len(l.entries) >= l.maxKeys) {
		for key, entry := range l.entries {
			if now.Sub(entry.started) >= l.window {
				delete(l.entries, key)
			}
		}
		l.swept = now
	}
	entry, ok := l.entries[ip]
	if !ok || now.Sub(entry.started) >= l.window {
		if !ok && l.maxKeys > 0 && len(l.entries) >= l.maxKeys {
			return false
		}
		l.entries[ip] = partyRateEntry{started: now, count: 1}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	l.entries[ip] = entry
	return true
}

// ClientIP returns the IP used for party rate limiting.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type partyTrack struct {
	VideoID      string  `json:"videoId"`
	Title        string  `json:"title"`
	Artist       string  `json:"artist"`
	ThumbnailURL *string `json:"thumbnailUrl,omitempty"`
	DurationMs   *int64  `json:"durationMs,omitempty"`
	FromAutoplay bool    `json:"fromAutoplay"`
}

func trackFromWire(raw any) *partyTrack {
	m, ok := raw.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	videoID, _ := m["videoId"].(string)
	if videoID == "" {
		return nil
	}
	if len(videoID) > 128 {
		videoID = videoID[:128]
	}
	title, _ := m["title"].(string)
	if len(title) > 300 {
		title = title[:300]
	}
	artist, _ := m["artist"].(string)
	if len(artist) > 300 {
		artist = artist[:300]
	}
	t := &partyTrack{
		VideoID: videoID,
		Title:   title,
		Artist:  artist,
	}
	if thumb, ok := m["thumbnailUrl"].(string); ok && thumb != "" {
		if len(thumb) > 1000 {
			thumb = thumb[:1000]
		}
		t.ThumbnailURL = &thumb
	}
	if duration, ok := m["durationMs"].(float64); ok && duration > 0 {
		value := int64(duration)
		t.DurationMs = &value
	}
	t.FromAutoplay, _ = m["fromAutoplay"].(bool)
	return t
}

type partyPlayback struct {
	Track           *partyTrack
	Queue           []*partyTrack
	QueueIndex      int
	IsPlaying       bool
	PositionMs      int64
	AnchorMs        int64
	Seq             int
	QueueSeq        int
	UpdatedBy       *string
	UpdatedAtMs     int64
	StartedBy       *string
	StartedByName   *string
	AutoplayEnabled bool

	clock            *partyClock
	playLeadMs       int64
	maxUpcomingQueue int
	maxQueueLength   int
}

func newPartyPlayback(clock *partyClock, cfg *config.Config) *partyPlayback {
	now := clock.NowMs()
	return &partyPlayback{
		Queue:            make([]*partyTrack, 0),
		QueueIndex:       -1,
		AnchorMs:         now,
		UpdatedAtMs:      now,
		clock:            clock,
		playLeadMs:       int64(cfg.PartyPlayLeadMS),
		maxUpcomingQueue: cfg.PartyMaxUpcomingQueue,
		maxQueueLength:   cfg.PartyMaxQueueLength,
	}
}

func (p *partyPlayback) positionAt(serverMs int64) int64 {
	if !p.IsPlaying {
		return p.PositionMs
	}
	elapsed := serverMs - p.AnchorMs
	if elapsed < 0 {
		elapsed = 0
	}
	position := p.PositionMs + elapsed
	if p.Track != nil && p.Track.DurationMs != nil && *p.Track.DurationMs > 0 && position > *p.Track.DurationMs {
		return *p.Track.DurationMs
	}
	return position
}

func (p *partyPlayback) touch(memberID *string) {
	now := p.clock.NowMs()
	p.Seq++
	p.UpdatedBy = memberID
	p.UpdatedAtMs = now
}

func (p *partyPlayback) touchQueue(memberID *string) {
	now := p.clock.NowMs()
	p.QueueSeq++
	p.UpdatedBy = memberID
	p.UpdatedAtMs = now
}

func (p *partyPlayback) play(memberID *string, positionMs *int64) {
	now := p.clock.NowMs()
	startAt := p.positionAt(now)
	if positionMs != nil {
		startAt = *positionMs
	}
	if startAt < 0 {
		startAt = 0
	}
	p.PositionMs = startAt
	p.IsPlaying = true
	p.AnchorMs = now + p.playLeadMs
	p.touch(memberID)
}

func (p *partyPlayback) pause(memberID *string, positionMs *int64) {
	now := p.clock.NowMs()
	pauseAt := p.positionAt(now)
	if positionMs != nil {
		pauseAt = *positionMs
	}
	if pauseAt < 0 {
		pauseAt = 0
	}
	p.PositionMs = pauseAt
	p.IsPlaying = false
	p.AnchorMs = now
	p.touch(memberID)
}

func (p *partyPlayback) seek(memberID *string, positionMs int64) {
	if positionMs < 0 {
		positionMs = 0
	}
	p.PositionMs = positionMs
	lead := int64(0)
	if p.IsPlaying {
		lead = p.playLeadMs
	}
	p.AnchorMs = p.clock.NowMs() + lead
	p.touch(memberID)
}

func (p *partyPlayback) setTrack(memberID *string, track *partyTrack, positionMs int64, isPlaying bool, queueIndex *int, memberName *string) {
	if positionMs < 0 {
		positionMs = 0
	}
	p.Track = track
	p.PositionMs = positionMs
	p.IsPlaying = isPlaying && track != nil
	lead := int64(0)
	if p.IsPlaying {
		lead = p.playLeadMs
	}
	p.AnchorMs = p.clock.NowMs() + lead
	if queueIndex != nil && *queueIndex >= 0 && *queueIndex < len(p.Queue) && track != nil && p.Queue[*queueIndex].VideoID == track.VideoID {
		p.QueueIndex = *queueIndex
	} else if track != nil {
		p.QueueIndex = partyNearestIndex(p.Queue, track.VideoID, p.QueueIndex)
	} else if queueIndex != nil {
		p.QueueIndex = *queueIndex
	}
	p.StartedBy = memberID
	p.StartedByName = memberName
	p.touch(memberID)
}

func partyNearestIndex(queue []*partyTrack, videoID string, around int) int {
	if around+1 >= 0 && around+1 < len(queue) && queue[around+1].VideoID == videoID {
		return around + 1
	}
	if around-1 >= 0 && around-1 < len(queue) && queue[around-1].VideoID == videoID {
		return around - 1
	}
	best := -1
	bestDistance := 0
	for i, item := range queue {
		if item.VideoID != videoID {
			continue
		}
		distance := i - around
		if distance < 0 {
			distance = -distance*2 + 1
		}
		if best == -1 || distance < bestDistance {
			best = i
			bestDistance = distance
		}
	}
	return best
}

func (p *partyPlayback) setQueue(memberID *string, queue []*partyTrack, queueIndex int) {
	if p.Track != nil {
		if queueIndex < 0 || queueIndex >= len(queue) || queue[queueIndex].VideoID != p.Track.VideoID {
			queueIndex = partyNearestIndex(queue, p.Track.VideoID, queueIndex)
		}
	}
	if queueIndex >= 0 && queueIndex < len(queue) {
		pastAndCurrent := queue[:queueIndex+1]
		endUpcoming := queueIndex + 1 + p.maxUpcomingQueue
		if endUpcoming > len(queue) {
			endUpcoming = len(queue)
		}
		upcoming := queue[queueIndex+1 : endUpcoming]
		newQueue := make([]*partyTrack, 0, len(pastAndCurrent)+len(upcoming))
		newQueue = append(newQueue, pastAndCurrent...)
		newQueue = append(newQueue, upcoming...)
		p.Queue = newQueue
		p.QueueIndex = queueIndex
	} else {
		maxLen := p.maxQueueLength
		if len(queue) < maxLen {
			maxLen = len(queue)
		}
		p.Queue = queue[:maxLen]
		if queueIndex >= 0 && queueIndex < len(p.Queue) {
			p.QueueIndex = queueIndex
		} else {
			p.QueueIndex = -1
		}
	}
	p.touchQueue(memberID)
}

func partyWithoutQueuedAutoplay(queue []*partyTrack, queueIndex int, tracks []*partyTrack) []*partyTrack {
	waiting := make(map[string]bool)
	for i := queueIndex + 1; i < len(queue); i++ {
		if i >= 0 && queue[i] != nil {
			waiting[queue[i].VideoID] = true
		}
	}
	out := make([]*partyTrack, 0, len(tracks))
	for _, track := range tracks {
		if track == nil {
			continue
		}
		if track.FromAutoplay {
			if waiting[track.VideoID] {
				continue
			}
			waiting[track.VideoID] = true
		}
		out = append(out, track)
	}
	return out
}

func (p *partyPlayback) addUpcoming(memberID *string, tracks []*partyTrack, playNext bool) (bool, string) {
	currentUpcoming := 0
	if p.QueueIndex >= 0 {
		currentUpcoming = len(p.Queue) - 1 - p.QueueIndex
		if currentUpcoming < 0 {
			currentUpcoming = 0
		}
	} else {
		currentUpcoming = len(p.Queue)
	}
	slotsLeft := p.maxUpcomingQueue - currentUpcoming
	if slotsLeft <= 0 {
		return false, "queue_full"
	}
	toAdd := partyWithoutQueuedAutoplay(p.Queue, p.QueueIndex, tracks)
	if len(toAdd) == 0 && len(tracks) > 0 {
		return true, ""
	}
	if len(toAdd) > slotsLeft {
		toAdd = toAdd[:slotsLeft]
	}
	if len(toAdd) == 0 {
		return false, "no_tracks"
	}
	if playNext && p.QueueIndex >= 0 && p.QueueIndex < len(p.Queue) {
		insertAt := p.QueueIndex + 1
		newQueue := make([]*partyTrack, 0, len(p.Queue)+len(toAdd))
		newQueue = append(newQueue, p.Queue[:insertAt]...)
		newQueue = append(newQueue, toAdd...)
		newQueue = append(newQueue, p.Queue[insertAt:]...)
		p.Queue = newQueue
	} else {
		p.Queue = append(p.Queue, toAdd...)
	}
	p.touchQueue(memberID)
	return true, ""
}

func (p *partyPlayback) removeUpcoming(memberID *string, videoID string) bool {
	match := -1
	for i, item := range p.Queue {
		if item.VideoID == videoID {
			match = i
			break
		}
	}
	if match == -1 || match == p.QueueIndex {
		return false
	}
	p.Queue = append(p.Queue[:match], p.Queue[match+1:]...)
	if p.QueueIndex > match {
		p.QueueIndex--
	}
	p.touchQueue(memberID)
	return true
}

func (p *partyPlayback) clearUpcoming(memberID *string) bool {
	if p.QueueIndex >= 0 {
		if len(p.Queue) <= p.QueueIndex+1 {
			return false
		}
		p.Queue = p.Queue[:p.QueueIndex+1]
	} else {
		if len(p.Queue) == 0 {
			return false
		}
		p.Queue = p.Queue[:0]
	}
	p.touchQueue(memberID)
	return true
}

func (p *partyPlayback) moveUpcoming(memberID *string, fromIndex, toIndex int, videoID string) bool {
	if videoID != "" {
		match := -1
		for i, item := range p.Queue {
			if item.VideoID == videoID {
				match = i
				break
			}
		}
		if match == -1 {
			return false
		}
		fromIndex = match
	}
	if fromIndex < 0 || fromIndex >= len(p.Queue) || toIndex < 0 || toIndex >= len(p.Queue) {
		return false
	}
	if fromIndex <= p.QueueIndex || toIndex <= p.QueueIndex {
		return false
	}
	if fromIndex == toIndex {
		return true
	}
	item := p.Queue[fromIndex]
	p.Queue = append(p.Queue[:fromIndex], p.Queue[fromIndex+1:]...)
	newQueue := make([]*partyTrack, 0, len(p.Queue)+1)
	newQueue = append(newQueue, p.Queue[:toIndex]...)
	newQueue = append(newQueue, item)
	newQueue = append(newQueue, p.Queue[toIndex:]...)
	p.Queue = newQueue
	p.touchQueue(memberID)
	return true
}

func (p *partyPlayback) step(memberID *string, delta int, memberName *string) bool {
	target := p.QueueIndex + delta
	if target < 0 || target >= len(p.Queue) {
		return false
	}
	p.setTrack(memberID, p.Queue[target], 0, true, &target, memberName)
	return true
}

func (p *partyPlayback) setAutoplay(memberID *string, enabled bool) {
	if p.AutoplayEnabled == enabled {
		return
	}
	p.AutoplayEnabled = enabled
	p.UpdatedBy = memberID
	p.UpdatedAtMs = p.clock.NowMs()
}

func (p *partyPlayback) toWire(serverMs int64) map[string]any {
	var track any
	if p.Track != nil {
		track = p.Track
	}
	return map[string]any{
		"seq":                 p.Seq,
		"track":               track,
		"queueSeq":            p.QueueSeq,
		"queueIndex":          p.QueueIndex,
		"queueLength":         len(p.Queue),
		"isPlaying":           p.IsPlaying,
		"positionMs":          p.PositionMs,
		"anchorMs":            p.AnchorMs,
		"effectivePositionMs": p.positionAt(serverMs),
		"updatedBy":           p.UpdatedBy,
		"startedBy":           p.StartedBy,
		"startedByName":       p.StartedByName,
		"autoplayEnabled":     p.AutoplayEnabled,
		"updatedAtMs":         p.UpdatedAtMs,
	}
}

func (p *partyPlayback) queueToWire() map[string]any {
	return map[string]any{
		"seq":   p.QueueSeq,
		"index": p.QueueIndex,
		"items": p.Queue,
	}
}

type partyMember struct {
	MemberID          string  `json:"memberId"`
	UserID            string  `json:"userId"`
	DeviceID          string  `json:"-"`
	DisplayName       string  `json:"displayName"`
	AvatarURL         *string `json:"avatarUrl,omitempty"`
	Token             string  `json:"-"`
	IsHost            bool    `json:"isHost"`
	Connected         bool    `json:"connected"`
	JoinedAtMs        int64   `json:"joinedAtMs"`
	LastSeenMs        int64   `json:"lastSeenMs"`
	ControlBudget     float64 `json:"-"`
	ControlBudgetAtMs int64   `json:"-"`
	FrameBudget       float64 `json:"-"`
	FrameBudgetAtMs   int64   `json:"-"`
}

func (m *partyMember) toWire() map[string]any {
	return map[string]any{
		"memberId":    m.MemberID,
		"userId":      m.UserID,
		"displayName": m.DisplayName,
		"avatarUrl":   m.AvatarURL,
		"isHost":      m.IsHost,
		"connected":   m.Connected,
		"joinedAtMs":  m.JoinedAtMs,
		"lastSeenMs":  m.LastSeenMs,
	}
}

type party struct {
	mu              sync.Mutex
	code            string
	members         map[string]*partyMember
	playback        *partyPlayback
	maxMembers      int
	hostOnlyControl bool
	createdAtMs     int64
	touchedAtMs     int64
	emptySinceMs    *int64

	clock             *partyClock
	disconnectGraceMS int64
	emptyTTLMS        int64
	partyMaxAgeMS     int64
	controlRatePerSec float64
	frameRatePerSec   float64
}

func newParty(code string, cfg *config.Config, clock *partyClock) *party {
	maxMembers := cfg.PartyMaxMembers
	if maxMembers < 2 || maxMembers > 10 {
		maxMembers = 5
	}
	now := clock.NowMs()
	emptySince := now
	return &party{
		code:              code,
		members:           make(map[string]*partyMember),
		playback:          newPartyPlayback(clock, cfg),
		maxMembers:        maxMembers,
		createdAtMs:       now,
		touchedAtMs:       now,
		emptySinceMs:      &emptySince,
		clock:             clock,
		disconnectGraceMS: cfg.PartyDisconnectGraceMS,
		emptyTTLMS:        cfg.PartyEmptyTTLMS,
		partyMaxAgeMS:     cfg.PartyMaxAgeMS,
		controlRatePerSec: float64(cfg.PartyControlRatePerSecond),
		frameRatePerSec:   float64(cfg.PartyFrameRatePerSecond),
	}
}

func (p *party) touch() {
	p.touchedAtMs = p.clock.NowMs()
}

func (p *party) join(userID, deviceID, displayName string, avatarURL *string) (*partyMember, *partyError) {
	now := p.clock.NowMs()
	for _, member := range p.members {
		if member.DeviceID == deviceID {
			member.DisplayName = displayName
			member.AvatarURL = avatarURL
			member.UserID = userID
			member.LastSeenMs = now
			member.Token = randomPartyToken(24)
			p.touch()
			return member, nil
		}
	}
	if len(p.members) >= p.maxMembers {
		return nil, newPartyError(409, "party_full", fmt.Sprintf("This party is full (%d devices).", p.maxMembers))
	}
	member := &partyMember{
		MemberID:          randomPartyHex(8),
		UserID:            userID,
		DeviceID:          deviceID,
		DisplayName:       displayName,
		AvatarURL:         avatarURL,
		Token:             randomPartyToken(24),
		IsHost:            len(p.members) == 0,
		Connected:         false,
		JoinedAtMs:        now,
		LastSeenMs:        now,
		ControlBudget:     p.controlRatePerSec,
		ControlBudgetAtMs: now,
		FrameBudget:       p.frameRatePerSec,
		FrameBudgetAtMs:   now,
	}
	p.members[member.MemberID] = member
	p.touch()
	return member, nil
}

func (p *party) authenticate(token string) (*partyMember, *partyError) {
	for _, member := range p.members {
		if subtle.ConstantTimeCompare([]byte(member.Token), []byte(token)) == 1 {
			return member, nil
		}
	}
	return nil, newPartyError(401, "bad_token", "This device is not a member of that party.")
}

func (p *party) remove(memberID string) *partyMember {
	member, ok := p.members[memberID]
	if !ok {
		return nil
	}
	delete(p.members, memberID)
	if member.IsHost && len(p.members) > 0 {
		for _, next := range p.members {
			next.IsHost = true
			break
		}
	}
	p.touch()
	p.refreshEmptiness()
	return member
}

func (p *party) mayControl(member *partyMember) bool {
	return !p.hostOnlyControl || member.IsHost
}

func (p *party) setMaxMembers(member *partyMember, maxMembers int) *partyError {
	if !member.IsHost {
		return newPartyError(403, "host_only", "Only the host can change the party size.")
	}
	if maxMembers < 2 || maxMembers > 10 {
		return newPartyError(422, "invalid_capacity", "Party size must be between 2 and 10.")
	}
	if maxMembers < len(p.members) {
		return newPartyError(409, "party_too_small", "Party size cannot be smaller than the current member count.")
	}
	p.maxMembers = maxMembers
	p.touch()
	return nil
}

func (p *party) setHostOnlyControl(member *partyMember, enabled bool) *partyError {
	if !member.IsHost {
		return newPartyError(403, "host_only", "Only the host can change who controls the music.")
	}
	p.hostOnlyControl = enabled
	p.touch()
	return nil
}

func (p *party) spendControlBudget(member *partyMember) bool {
	now := p.clock.NowMs()
	elapsed := float64(now-member.ControlBudgetAtMs) / 1000.0
	if elapsed < 0 {
		elapsed = 0
	}
	member.ControlBudget += elapsed * p.controlRatePerSec
	if member.ControlBudget > p.controlRatePerSec {
		member.ControlBudget = p.controlRatePerSec
	}
	member.ControlBudgetAtMs = now
	if member.ControlBudget < 1 {
		return false
	}
	member.ControlBudget--
	return true
}

func (p *party) spendFrameBudget(member *partyMember) bool {
	now := p.clock.NowMs()
	elapsed := float64(now-member.FrameBudgetAtMs) / 1000.0
	if elapsed < 0 {
		elapsed = 0
	}
	member.FrameBudget += elapsed * p.frameRatePerSec
	if member.FrameBudget > p.frameRatePerSec {
		member.FrameBudget = p.frameRatePerSec
	}
	member.FrameBudgetAtMs = now
	if member.FrameBudget < 1 {
		return false
	}
	member.FrameBudget--
	return true
}

func (p *party) markConnected(member *partyMember, connected bool) {
	member.Connected = connected
	member.LastSeenMs = p.clock.NowMs()
	p.touch()
	p.refreshEmptiness()
}

func (p *party) refreshEmptiness() {
	anyConnected := false
	for _, member := range p.members {
		if member.Connected {
			anyConnected = true
			break
		}
	}
	if anyConnected {
		p.emptySinceMs = nil
		return
	}
	if p.emptySinceMs == nil {
		now := p.clock.NowMs()
		p.emptySinceMs = &now
	}
}

func (p *party) expiredMembers(now int64) []*partyMember {
	var expired []*partyMember
	for _, member := range p.members {
		if !member.Connected && now-member.LastSeenMs > p.disconnectGraceMS {
			expired = append(expired, member)
		}
	}
	return expired
}

func (p *party) isExpired(now int64) bool {
	if now-p.createdAtMs > p.partyMaxAgeMS {
		return true
	}
	if p.emptySinceMs != nil && now-*p.emptySinceMs > p.emptyTTLMS {
		return true
	}
	return false
}

func (p *party) toWire() map[string]any {
	now := p.clock.NowMs()
	members := make([]*partyMember, 0, len(p.members))
	for _, member := range p.members {
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].JoinedAtMs < members[j].JoinedAtMs
	})
	memberWire := make([]map[string]any, 0, len(members))
	for _, member := range members {
		memberWire = append(memberWire, member.toWire())
	}
	return map[string]any{
		"code":            p.code,
		"createdAtMs":     p.createdAtMs,
		"maxMembers":      p.maxMembers,
		"hostOnlyControl": p.hostOnlyControl,
		"members":         memberWire,
		"playback":        p.playback.toWire(now),
		"queue":           p.playback.queueToWire(),
		"serverMs":        now,
	}
}

type partyStore struct {
	mu      sync.RWMutex
	parties map[string]*party
}

func newPartyStore() *partyStore {
	return &partyStore{parties: make(map[string]*party)}
}

func (s *partyStore) create(maxParties, maxMembers int, cfg *config.Config, clock *partyClock) (*party, *partyError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxParties > 0 && len(s.parties) >= maxParties {
		return nil, newPartyError(503, "server_full", "The party server is busy. Please try again in a few minutes.")
	}
	for i := 0; i < 12; i++ {
		code := newPartyCode()
		if _, exists := s.parties[code]; exists {
			continue
		}
		p := newParty(code, cfg, clock)
		if maxMembers >= 2 && maxMembers <= 10 {
			p.maxMembers = maxMembers
		}
		s.parties[code] = p
		return p, nil
	}
	return nil, newPartyError(503, "code_exhausted", "Could not allocate a party code.")
}

func (s *partyStore) get(code string) (*party, *partyError) {
	normalised := NormalisePartyCode(code)
	s.mu.RLock()
	p, ok := s.parties[normalised]
	s.mu.RUnlock()
	if !ok {
		return nil, newPartyError(404, "no_such_party", "No party with that code.")
	}
	return p, nil
}

func (s *partyStore) find(code string) *party {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parties[NormalisePartyCode(code)]
}

func (s *partyStore) drop(code string) {
	s.mu.Lock()
	delete(s.parties, NormalisePartyCode(code))
	s.mu.Unlock()
}

func (s *partyStore) all() []*party {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*party, 0, len(s.parties))
	for _, p := range s.parties {
		out = append(out, p)
	}
	return out
}

func (s *partyStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.parties)
}

func (s *partyStore) memberCount() int {
	parties := s.all()
	total := 0
	for _, p := range parties {
		p.mu.Lock()
		total += len(p.members)
		p.mu.Unlock()
	}
	return total
}

func (s *partyStore) sweep(now int64) (changed []*party, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for code, p := range s.parties {
		p.mu.Lock()
		expired := p.expiredMembers(now)
		for _, member := range expired {
			p.remove(member.MemberID)
		}
		if len(expired) > 0 {
			changed = append(changed, p)
		}
		expiredParty := p.isExpired(now)
		p.mu.Unlock()
		if expiredParty {
			delete(s.parties, code)
			removed = append(removed, code)
		}
	}
	return changed, removed
}

type partySafeConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *partySafeConn) writeJSON(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *partySafeConn) close() {
	c.mu.Lock()
	_ = c.conn.Close(websocket.StatusNormalClosure, "done")
	c.mu.Unlock()
}

type partyHub struct {
	mu      sync.RWMutex
	sockets map[string]map[string]*partySafeConn
}

func newPartyHub() *partyHub {
	return &partyHub{sockets: make(map[string]map[string]*partySafeConn)}
}

func (h *partyHub) attach(code, memberID string, conn *websocket.Conn) *partySafeConn {
	safe := &partySafeConn{conn: conn}
	h.mu.Lock()
	room, ok := h.sockets[code]
	if !ok {
		room = make(map[string]*partySafeConn)
		h.sockets[code] = room
	}
	previous := room[memberID]
	room[memberID] = safe
	h.mu.Unlock()
	if previous != nil && previous != safe {
		previous.close()
	}
	return safe
}

func (h *partyHub) detach(code, memberID string, safe *partySafeConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.sockets[code]
	if !ok {
		return
	}
	if room[memberID] == safe {
		delete(room, memberID)
	}
	if len(room) == 0 {
		delete(h.sockets, code)
	}
}

func (h *partyHub) dropParty(code string) {
	h.mu.Lock()
	room := h.sockets[code]
	delete(h.sockets, code)
	h.mu.Unlock()
	for _, safe := range room {
		safe.close()
	}
}

func (h *partyHub) membersOnline(code string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.sockets[code]
	out := make([]string, 0, len(room))
	for memberID := range room {
		out = append(out, memberID)
	}
	return out
}

func (h *partyHub) activeCodes() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.sockets))
	for code := range h.sockets {
		out = append(out, code)
	}
	return out
}

func (h *partyHub) send(code, memberID string, payload any) {
	h.mu.RLock()
	room := h.sockets[code]
	var target *partySafeConn
	if room != nil {
		target = room[memberID]
	}
	h.mu.RUnlock()
	if target != nil {
		_ = target.writeJSON(payload)
	}
}

func (h *partyHub) closeMember(code, memberID string) {
	h.mu.RLock()
	room := h.sockets[code]
	var target *partySafeConn
	if room != nil {
		target = room[memberID]
	}
	h.mu.RUnlock()
	if target != nil {
		target.close()
	}
}

func (h *partyHub) broadcast(code string, payload any, skipMemberID string) {
	h.mu.RLock()
	room := h.sockets[code]
	if len(room) == 0 {
		h.mu.RUnlock()
		return
	}
	targets := make([]*partySafeConn, 0, len(room))
	for memberID, safe := range room {
		if memberID == skipMemberID {
			continue
		}
		targets = append(targets, safe)
	}
	h.mu.RUnlock()
	var wg sync.WaitGroup
	for _, safe := range targets {
		wg.Add(1)
		go func(conn *partySafeConn) {
			defer wg.Done()
			_ = conn.writeJSON(payload)
		}(safe)
	}
	wg.Wait()
}

type partyPresenceCounts = models.PartyLiveStats

type partyPresenceEntry struct {
	lastMs   int64
	platform uint8
}

type partyPresenceTracker struct {
	mu         sync.Mutex
	seen       map[[16]byte]partyPresenceEntry
	windowMS   int64
	maxEntries int
	counts     partyPresenceCounts
}

func newPartyPresenceTracker(window time.Duration, maxEntries int) *partyPresenceTracker {
	return &partyPresenceTracker{
		seen:       make(map[[16]byte]partyPresenceEntry),
		windowMS:   window.Milliseconds(),
		maxEntries: maxEntries,
	}
}

func parsePartyUUID(value string) ([16]byte, bool) {
	var id [16]byte
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return id, false
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	if _, err := hex.DecodeString(compact); err != nil {
		return id, false
	}
	if _, err := hex.Decode(id[:], []byte(compact)); err != nil {
		return id, false
	}
	return id, true
}

func parsePartyPlatform(value string) (uint8, bool) {
	switch value {
	case "android":
		return 1, true
	case "pc":
		return 2, true
	default:
		return 0, false
	}
}

func (t *partyPresenceTracker) ping(id [16]byte, platform uint8, nowMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, known := t.seen[id]; !known && t.maxEntries > 0 && len(t.seen) >= t.maxEntries {
		return
	}
	t.seen[id] = partyPresenceEntry{lastMs: nowMs, platform: platform}
}

func (t *partyPresenceTracker) close(id [16]byte) {
	t.mu.Lock()
	delete(t.seen, id)
	t.mu.Unlock()
}

func (t *partyPresenceTracker) sweep(nowMs int64) {
	cutoff := nowMs - t.windowMS
	counts := partyPresenceCounts{UpdatedAt: nowMs}
	t.mu.Lock()
	for id, entry := range t.seen {
		if entry.lastMs < cutoff {
			delete(t.seen, id)
			continue
		}
		switch entry.platform {
		case 1:
			counts.Android++
		case 2:
			counts.PC++
		}
	}
	t.mu.Unlock()
	counts.Online = counts.Android + counts.PC
	t.counts = counts
}

func (t *partyPresenceTracker) snapshot() partyPresenceCounts {
	return t.counts
}

// Health returns basic service statistics.
func (s *PartyService) Health() map[string]any {
	return map[string]any{
		"service":  "streamx-listen-together",
		"members":  s.store.memberCount(),
		"parties":  s.store.len(),
		"serverMs": s.clock.NowMs(),
	}
}

// NowMs returns a monotonic server timestamp.
func (s *PartyService) NowMs() int64 {
	return s.clock.NowMs()
}

// RequestMaxBytes returns the configured REST request body limit.
func (s *PartyService) RequestMaxBytes() int64 {
	return s.cfg.PartyRequestMaxBytes
}

// CreateParty creates a new party and joins the creator as host.
func (s *PartyService) CreateParty(clientIP string, req *models.PartyJoinRequest) (*models.PartyAuthResponse, *partyError) {
	if !s.createLimiter.Allow(clientIP) {
		return nil, newPartyError(429, "create_rate_limited", "You can create up to two parties per minute. Please try again shortly.")
	}
	if err := validatePartyJoinRequest(req); err != nil {
		return nil, newPartyError(422, "validation_error", err.Error())
	}
	maxMembers := s.cfg.PartyMaxMembers
	if req.MaxMembers != nil {
		maxMembers = *req.MaxMembers
	}
	p, perr := s.store.create(s.cfg.PartyMaxParties, maxMembers, s.cfg, s.clock)
	if perr != nil {
		return nil, perr
	}
	p.mu.Lock()
	if req.AutoplayEnabled != nil {
		p.playback.AutoplayEnabled = *req.AutoplayEnabled
	}
	member, jerr := p.join(req.UserID, req.DeviceID, req.DisplayName, req.AvatarURL)
	if jerr != nil {
		p.mu.Unlock()
		s.store.drop(p.code)
		return nil, jerr
	}
	response := &models.PartyAuthResponse{
		Code:  p.code,
		Token: member.Token,
		You:   member.toWire(),
		Party: p.toWire(),
	}
	p.mu.Unlock()
	return response, nil
}

// JoinParty joins an existing party.
func (s *PartyService) JoinParty(code string, req *models.PartyJoinRequest) (*models.PartyAuthResponse, *partyError) {
	if err := validatePartyJoinRequest(req); err != nil {
		return nil, newPartyError(422, "validation_error", err.Error())
	}
	p, perr := s.store.get(code)
	if perr != nil {
		return nil, perr
	}
	p.mu.Lock()
	member, jerr := p.join(req.UserID, req.DeviceID, req.DisplayName, req.AvatarURL)
	if jerr != nil {
		p.mu.Unlock()
		return nil, jerr
	}
	response := &models.PartyAuthResponse{
		Code:  p.code,
		Token: member.Token,
		You:   member.toWire(),
		Party: p.toWire(),
	}
	p.mu.Unlock()
	return response, nil
}

// GetParty returns the authenticated full party snapshot.
func (s *PartyService) GetParty(code, token string) (map[string]any, *partyError) {
	if token == "" {
		return nil, newPartyError(401, "unauthorized", "Missing bearer token.")
	}
	p, perr := s.store.get(code)
	if perr != nil {
		return nil, perr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, perr := p.authenticate(token); perr != nil {
		return nil, perr
	}
	return p.toWire(), nil
}

// PreviewParty returns public party membership information.
func (s *PartyService) PreviewParty(code string) (*models.PartyPreview, *partyError) {
	p, perr := s.store.get(code)
	if perr != nil {
		return nil, perr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	members := make([]*partyMember, 0, len(p.members))
	for _, member := range p.members {
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].JoinedAtMs < members[j].JoinedAtMs
	})
	preview := &models.PartyPreview{
		Code:        p.code,
		MemberCount: len(p.members),
		MaxMembers:  p.maxMembers,
		IsFull:      len(p.members) >= p.maxMembers,
		Members:     make([]models.PartyPreviewMember, 0, len(members)),
	}
	for _, member := range members {
		preview.Members = append(preview.Members, models.PartyPreviewMember{
			DisplayName: member.DisplayName,
			AvatarURL:   member.AvatarURL,
			IsHost:      member.IsHost,
		})
		if member.IsHost && preview.HostName == "" {
			preview.HostName = member.DisplayName
		}
	}
	return preview, nil
}

// PartyInviteInfo is the public state shown on the /invite/{code} landing page.
type PartyInviteInfo struct {
	Active      bool
	MemberCount int
	Title       string
	Artist      string
}

// InviteInfo returns public invite-page information without joining the party.
func (s *PartyService) InviteInfo(code string) PartyInviteInfo {
	p := s.store.find(code)
	if p == nil {
		return PartyInviteInfo{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	info := PartyInviteInfo{Active: true, MemberCount: len(p.members)}
	if p.playback.Track != nil {
		info.Title = p.playback.Track.Title
		info.Artist = p.playback.Track.Artist
	}
	return info
}

// LeaveParty removes the authenticated member from the party.
func (s *PartyService) LeaveParty(code, token string) (map[string]any, *partyError) {
	if token == "" {
		return nil, newPartyError(401, "unauthorized", "Missing bearer token.")
	}
	p, perr := s.store.get(code)
	if perr != nil {
		return nil, perr
	}
	p.mu.Lock()
	member, perr := p.authenticate(token)
	if perr != nil {
		p.mu.Unlock()
		return nil, perr
	}
	memberID := member.MemberID
	p.remove(memberID)
	membersFrame := s.membersFrame(p)
	p.mu.Unlock()

	s.hub.send(p.code, memberID, map[string]any{
		"type":   frameBye,
		"reason": "left",
	})
	s.hub.broadcast(p.code, membersFrame, memberID)
	return map[string]any{"ok": true}, nil
}

// AuthenticateSocket validates a party token before a WebSocket upgrade.
func (s *PartyService) AuthenticateSocket(code, token string) (*party, *partyMember, *partyError) {
	if token == "" {
		return nil, nil, newPartyError(401, "unauthorized", "missing token")
	}
	p, perr := s.store.get(code)
	if perr != nil {
		return nil, nil, newPartyError(404, "no_such_party", "no such party")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	member, perr := p.authenticate(token)
	if perr != nil {
		return nil, nil, newPartyError(401, "bad_token", "bad token")
	}
	return p, member, nil
}

// ServeWebSocket owns the party WebSocket read loop.
func (s *PartyService) ServeWebSocket(ctx context.Context, p *party, member *partyMember, conn *websocket.Conn) {
	conn.SetReadLimit(s.cfg.PartyWebSocketMaxBytes)
	safe := s.hub.attach(p.code, member.MemberID, conn)

	p.mu.Lock()
	p.markConnected(member, true)
	welcome := map[string]any{
		"type":     frameWelcome,
		"you":      member.toWire(),
		"party":    p.toWire(),
		"serverMs": s.clock.NowMs(),
	}
	membersFrame := s.membersFrame(p)
	p.mu.Unlock()

	_ = safe.writeJSON(welcome)
	s.hub.broadcast(p.code, membersFrame, "")

	defer func() {
		s.hub.detach(p.code, member.MemberID, safe)
		safe.close()
		p.mu.Lock()
		if current, perr := p.authenticate(member.Token); perr == nil {
			p.markConnected(current, false)
		}
		frame := s.membersFrame(p)
		p.mu.Unlock()
		s.hub.broadcast(p.code, frame, "")
	}()

	idle := time.Duration(s.cfg.PartyConnectionIdleMS) * time.Millisecond
	for {
		readCtx, cancel := context.WithTimeout(ctx, idle)
		messageType, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil || len(frame) == 0 {
			continue
		}
		s.handleSocketFrame(p, member, safe, frame)
	}
}

// HandlePresence records an open-app heartbeat.
func (s *PartyService) HandlePresence(clientIP string, req *models.PresenceRequest) (map[string]any, *partyError) {
	if req == nil {
		return nil, newPartyError(422, "invalid_json", "Malformed JSON body.")
	}
	if !s.presenceLimiter.Allow(clientIP) {
		return nil, newPartyError(429, "rate_limited", "Too many presence pings from this address.")
	}
	id, ok := parsePartyUUID(req.ID)
	if !ok {
		return nil, newPartyError(422, "invalid_id", "id must be a UUID.")
	}
	platform, ok := parsePartyPlatform(req.Platform)
	if !ok {
		return nil, newPartyError(422, "invalid_platform", "platform must be android or pc.")
	}
	if req.Open == nil {
		return nil, newPartyError(422, "invalid_open", "open must be true or false.")
	}
	now := s.clock.NowMs()
	if *req.Open {
		s.presence.ping(id, platform, now)
	} else {
		s.presence.close(id)
	}
	return map[string]any{"intervalSec": s.cfg.PartyPresenceIntervalSec}, nil
}

// LiveStats returns the current open-app counts.
func (s *PartyService) LiveStats() any {
	return s.presence.snapshot()
}

// StartHeartbeat re-states playback truth to every active room.
func (s *PartyService) StartHeartbeat(ctx context.Context) {
	interval := time.Duration(s.cfg.PartyStateHeartbeatMS) * time.Millisecond
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range s.store.all() {
				if len(s.hub.membersOnline(p.code)) == 0 {
					continue
				}
				p.mu.Lock()
				frame := s.stateFrame(p)
				p.mu.Unlock()
				s.hub.broadcast(p.code, frame, "")
			}
		}
	}
}

// StartSweeper removes expired members/parties and refreshes presence counts.
func (s *PartyService) StartSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := s.clock.NowMs()
			changed, removed := s.store.sweep(now)
			for _, p := range changed {
				p.mu.Lock()
				frame := s.membersFrame(p)
				p.mu.Unlock()
				s.hub.broadcast(p.code, frame, "")
			}
			for _, code := range removed {
				s.hub.dropParty(code)
			}
			active := make(map[string]bool, len(s.store.all()))
			for _, p := range s.store.all() {
				active[p.code] = true
			}
			for _, code := range s.hub.activeCodes() {
				if !active[code] {
					s.hub.dropParty(code)
				}
			}
			s.presence.sweep(now)
		}
	}
}

func (s *PartyService) stateFrame(p *party) map[string]any {
	return map[string]any{
		"type":     frameState,
		"playback": p.playback.toWire(s.clock.NowMs()),
		"serverMs": s.clock.NowMs(),
	}
}

func (s *PartyService) queueFrame(p *party) map[string]any {
	return map[string]any{
		"type":     frameQueue,
		"queue":    p.playback.queueToWire(),
		"serverMs": s.clock.NowMs(),
	}
}

func (s *PartyService) membersFrame(p *party) map[string]any {
	members := make([]map[string]any, 0, len(p.members))
	for _, member := range p.members {
		members = append(members, member.toWire())
	}
	return map[string]any{
		"type":            frameMembers,
		"members":         members,
		"maxMembers":      p.maxMembers,
		"hostOnlyControl": p.hostOnlyControl,
		"serverMs":        s.clock.NowMs(),
	}
}

func (s *PartyService) activityFrame(member *partyMember, action string, frame map[string]any) map[string]any {
	detail := action
	titleOf := func(raw any) string {
		track, _ := raw.(map[string]any)
		title, _ := track["title"].(string)
		return title
	}
	switch action {
	case actionSetTrack:
		if title := titleOf(frame["track"]); title != "" {
			detail = "Changed the song to \"" + title + "\""
		}
	case actionQueueAdd:
		if tracks, ok := frame["tracks"].([]any); ok && len(tracks) > 0 {
			if title := titleOf(tracks[0]); title != "" {
				if len(tracks) == 1 {
					detail = "Added \"" + title + "\" to the queue"
				} else {
					detail = fmt.Sprintf("Added \"%s\" and %d more to the queue", title, len(tracks)-1)
				}
			}
		}
	case actionSetQueue:
		if tracks, ok := frame["queue"].([]any); ok {
			detail = fmt.Sprintf("Replaced the queue with %d tracks", len(tracks))
		}
	case actionQueueRemove:
		detail = "Removed a track from the queue"
	case actionQueueClear:
		detail = "Cleared upcoming tracks"
	case actionQueueMove:
		detail = "Reordered the upcoming queue"
	case actionNext:
		detail = "Skipped to the next song"
	case actionPrevious:
		detail = "Went back to the previous song"
	case actionPlay:
		detail = "Started playback"
	case actionPause:
		detail = "Paused playback"
	case actionSeek:
		detail = "Changed the playback position"
	case actionKick:
		detail = "Removed a listener from the party"
	case actionSetMax:
		detail = "Changed the party size"
	case actionSetAutoplay:
		detail = "Changed AutoPlay"
	}
	return map[string]any{
		"type":   frameActivity,
		"action": action,
		"by":     member.DisplayName,
		"detail": detail,
		"atMs":   s.clock.NowMs(),
	}
}

func (s *PartyService) handleSocketFrame(p *party, member *partyMember, safe *partySafeConn, frame map[string]any) {
	kind, _ := frame["type"].(string)
	if kind == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.spendFrameBudget(member) {
		_ = safe.writeJSON(map[string]any{
			"type":    frameError,
			"error":   "rate_limited",
			"message": "Too many messages at once.",
		})
		return
	}
	member.LastSeenMs = s.clock.NowMs()

	switch kind {
	case framePing:
		_ = safe.writeJSON(map[string]any{
			"type":     framePong,
			"clientMs": frame["clientMs"],
			"serverMs": s.clock.NowMs(),
		})
	case frameSync:
		_ = safe.writeJSON(s.stateFrame(p))
	case frameSyncQueue:
		_ = safe.writeJSON(s.queueFrame(p))
	case frameReport:
		position, ok := frame["positionMs"].(float64)
		if ok && p.playback.IsPlaying {
			drift := int64(position) - p.playback.positionAt(s.clock.NowMs())
			if drift > 1500 || drift < -1500 {
				partyLog.Infof("party %s: %s drifted %dms", p.code, member.DisplayName, drift)
			}
		}
	case frameControl:
		if !p.spendControlBudget(member) {
			_ = safe.writeJSON(map[string]any{
				"type":    frameError,
				"error":   "rate_limited",
				"message": "Too many controls at once.",
			})
			return
		}
		queueBefore := p.playback.QueueSeq
		action, _ := frame["action"].(string)
		success, code, message := s.applyControl(p, member, action, frame)
		if success {
			p.touch()
			if p.playback.QueueSeq != queueBefore {
				s.hub.broadcast(p.code, s.queueFrame(p), "")
			}
			s.hub.broadcast(p.code, s.stateFrame(p), "")
			if action == actionKick || action == actionSetMax || action == actionSetHostOnly {
				s.hub.broadcast(p.code, s.membersFrame(p), "")
			}
			s.hub.broadcast(p.code, s.activityFrame(member, action, frame), "")
		} else if code != "" {
			_ = safe.writeJSON(map[string]any{
				"type":    frameError,
				"error":   code,
				"message": message,
			})
		}
	}
}

func (s *PartyService) applyControl(p *party, member *partyMember, action string, frame map[string]any) (bool, string, string) {
	if partyControlActions[action] && !p.mayControl(member) {
		return false, "host_only", "Only the host can control the music in this party."
	}
	var position *int64
	if value, ok := frame["positionMs"].(float64); ok {
		parsed := int64(value)
		position = &parsed
	}
	switch action {
	case actionPlay:
		p.playback.play(&member.MemberID, position)
		return true, "", ""
	case actionPause:
		p.playback.pause(&member.MemberID, position)
		return true, "", ""
	case actionSeek:
		if position == nil {
			return false, "missing_position", "seek requires positionMs"
		}
		p.playback.seek(&member.MemberID, *position)
		return true, "", ""
	case actionSetTrack:
		track := trackFromWire(frame["track"])
		if track == nil {
			return false, "invalid_track", "Track must include videoId"
		}
		positionMs := int64(0)
		if position != nil {
			positionMs = *position
		}
		isPlaying := true
		if value, ok := frame["isPlaying"].(bool); ok {
			isPlaying = value
		}
		var queueIndex *int
		if value, ok := frame["queueIndex"].(float64); ok {
			index := int(value)
			queueIndex = &index
		}
		p.playback.setTrack(&member.MemberID, track, positionMs, isPlaying, queueIndex, &member.DisplayName)
		return true, "", ""
	case actionSetQueue:
		var tracks []*partyTrack
		if items, ok := frame["queue"].([]any); ok {
			for _, item := range items {
				if track := trackFromWire(item); track != nil {
					tracks = append(tracks, track)
				}
			}
		}
		index := -1
		if value, ok := frame["queueIndex"].(float64); ok {
			index = int(value)
		}
		p.playback.setQueue(&member.MemberID, tracks, index)
		return true, "", ""
	case actionQueueAdd:
		var tracks []*partyTrack
		if single, ok := frame["track"].(map[string]any); ok {
			if track := trackFromWire(single); track != nil {
				tracks = append(tracks, track)
			}
		} else if items, ok := frame["tracks"].([]any); ok {
			for _, item := range items {
				if track := trackFromWire(item); track != nil {
					tracks = append(tracks, track)
				}
			}
		}
		playNext, _ := frame["playNext"].(bool)
		ok, reason := p.playback.addUpcoming(&member.MemberID, tracks, playNext)
		if !ok {
			if reason == "queue_full" {
				return false, reason, fmt.Sprintf("Queue is full (maximum %d upcoming songs).", s.cfg.PartyMaxUpcomingQueue)
			}
			return false, reason, "Could not add track to queue."
		}
		return true, "", ""
	case actionQueueRemove:
		videoID, _ := frame["videoId"].(string)
		if videoID == "" {
			if value, ok := frame["index"].(float64); ok {
				index := int(value)
				if index >= 0 && index < len(p.playback.Queue) {
					videoID = p.playback.Queue[index].VideoID
				}
			}
		}
		if videoID == "" {
			return false, "missing_track", "queueRemove requires videoId or index"
		}
		if !p.playback.removeUpcoming(&member.MemberID, videoID) {
			return false, "not_found", "Track is not in the upcoming queue"
		}
		return true, "", ""
	case actionQueueClear:
		if !p.playback.clearUpcoming(&member.MemberID) {
			return false, "no_upcoming", "No upcoming tracks to clear"
		}
		return true, "", ""
	case actionQueueMove:
		from, okFrom := frame["fromIndex"].(float64)
		to, okTo := frame["toIndex"].(float64)
		if !okFrom || !okTo {
			return false, "missing_indices", "queueMove requires fromIndex and toIndex"
		}
		videoID, _ := frame["videoId"].(string)
		if !p.playback.moveUpcoming(&member.MemberID, int(from), int(to), videoID) {
			return false, "invalid_move", "Invalid queue move indices"
		}
		return true, "", ""
	case actionNext:
		if !p.playback.step(&member.MemberID, 1, &member.DisplayName) {
			return false, "end_of_queue", "Already at the end of the queue."
		}
		return true, "", ""
	case actionPrevious:
		if !p.playback.step(&member.MemberID, -1, &member.DisplayName) {
			return false, "start_of_queue", "Already at the beginning of the queue."
		}
		return true, "", ""
	case actionSetMax:
		value, ok := frame["maxMembers"].(float64)
		if !ok {
			return false, "invalid_capacity", "Choose a party size between 2 and 10."
		}
		if err := p.setMaxMembers(member, int(value)); err != nil {
			return false, err.Code, err.Message
		}
		return true, "", ""
	case actionSetAutoplay:
		enabled, ok := frame["enabled"].(bool)
		if !ok {
			return false, "invalid_autoplay", "AutoPlay must be enabled or disabled."
		}
		p.playback.setAutoplay(&member.MemberID, enabled)
		return true, "", ""
	case actionSetHostOnly:
		enabled, ok := frame["enabled"].(bool)
		if !ok {
			return false, "invalid_control_policy", "Host-only control must be enabled or disabled."
		}
		if err := p.setHostOnlyControl(member, enabled); err != nil {
			return false, err.Code, err.Message
		}
		return true, "", ""
	case actionKick:
		targetID, _ := frame["memberId"].(string)
		if !member.IsHost {
			return false, "host_only", "Only the host can remove listeners."
		}
		if targetID == "" || targetID == member.MemberID {
			return false, "invalid_member", "Choose another listener to remove."
		}
		if p.remove(targetID) == nil {
			return false, "not_found", "That listener is no longer in this party."
		}
		s.hub.send(p.code, targetID, map[string]any{
			"type":    frameBye,
			"reason":  "kicked",
			"message": "The host removed you from this party.",
		})
		s.hub.closeMember(p.code, targetID)
		return true, "", ""
	default:
		return false, "unknown_action", fmt.Sprintf("Unknown control action '%s'", action)
	}
}
