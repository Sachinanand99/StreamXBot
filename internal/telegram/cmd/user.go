package cmd

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// handlePing responds with latency and service uptime.
func (h *Handler) handlePing(ctx context.Context, e tg.Entities, upd message.AnswerableMessageUpdate, msg *tg.Message) {
	start := time.Now()
	uptime := time.Since(h.startTime)
	latency := time.Since(start).Milliseconds()
	if latency <= 0 {
		latency = 1
	}
	uptimeStr := formatDuration(uptime)
	reply := fmt.Sprintf("<code>%d</code>ms \n| <code>%s</code>", latency, uptimeStr)
	_ = h.replyHTML(ctx, e, upd, reply, nil)
}

// handleID displays chat ID, sender user ID, or replied user ID.
func (h *Handler) handleID(ctx context.Context, e tg.Entities, upd message.AnswerableMessageUpdate, msg *tg.Message, chatID int64, senderID int64) {
	replyUser := int64(0)
	if msg.ReplyTo != nil {
		if header, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && header.ReplyToMsgID != 0 {
			if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
				res, err := w.API.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: header.ReplyToMsgID}})
				if err == nil {
					switch mList := res.(type) {
					case *tg.MessagesMessages:
						for _, m := range mList.Messages {
							if fullMsg, ok := m.(*tg.Message); ok && fullMsg.FromID != nil {
								if u, ok := fullMsg.FromID.(*tg.PeerUser); ok {
									replyUser = u.UserID
									break
								}
							}
						}
					case *tg.MessagesMessagesSlice:
						for _, m := range mList.Messages {
							if fullMsg, ok := m.(*tg.Message); ok && fullMsg.FromID != nil {
								if u, ok := fullMsg.FromID.(*tg.PeerUser); ok {
									replyUser = u.UserID
									break
								}
							}
						}
					case *tg.MessagesChannelMessages:
						for _, m := range mList.Messages {
							if fullMsg, ok := m.(*tg.Message); ok && fullMsg.FromID != nil {
								if u, ok := fullMsg.FromID.(*tg.PeerUser); ok {
									replyUser = u.UserID
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if replyUser != 0 {
		_ = h.replyHTML(ctx, e, upd, fmt.Sprintf("Replied user ID: <code>%d</code>", replyUser), nil)
		return
	}

	text := fmt.Sprintf("Chat ID: <code>%d</code>", chatID)
	if senderID != 0 {
		text += fmt.Sprintf("\nYour User ID: <code>%d</code>", senderID)
	}

	_ = h.replyHTML(ctx, e, upd, text, nil)
}

// handleSearch queries tracks by title, artist, or album.
func (h *Handler) handleSearch(ctx context.Context, e tg.Entities, upd message.AnswerableMessageUpdate, msg *tg.Message, args []string) {
	if len(args) == 0 {
		_ = h.replyHTML(ctx, e, upd, "Usage:\n/search &lt;query&gt;", nil)
		return
	}

	query := strings.Join(args, " ")
	if h.trackSvc == nil {
		_ = h.replyHTML(ctx, e, upd, "Track search service unavailable.", nil)
		return
	}

	results, err := h.trackSvc.Search(ctx, query, 5)
	if err != nil || len(results) == 0 {
		_ = h.replyHTML(ctx, e, upd, "No results found.", nil)
		return
	}

	var sb strings.Builder
	for i, item := range results {
		trackName := html.EscapeString(item.Title)
		artist := html.EscapeString(item.Artist)
		album := html.EscapeString(item.Album)

		sb.WriteString(fmt.Sprintf("🎵 <code>%s</code>\n", trackName))
		if artist != "" {
			sb.WriteString(fmt.Sprintf("👤 %s\n", artist))
		}
		if album != "" {
			sb.WriteString(fmt.Sprintf("💿 %s\n", album))
		}
		durStr := formatSeconds(item.DurationSec)
		sb.WriteString(fmt.Sprintf("%s · <code>%s</code>\n", durStr, item.ID))

		if i < len(results)-1 {
			sb.WriteString("\n")
		}
	}

	_ = h.replyHTML(ctx, e, upd, sb.String(), nil)
}

// handleMediaInfo inspects technical metadata of an attached, replied, or recently sent audio/media file.
func (h *Handler) handleMediaInfo(ctx context.Context, e tg.Entities, upd message.AnswerableMessageUpdate, msg *tg.Message) {
	docFromMessage := func(m *tg.Message) *tg.Document {
		if m == nil || m.Media == nil {
			return nil
		}
		if docMedia, ok := m.Media.(*tg.MessageMediaDocument); ok && docMedia.Document != nil {
			doc, _ := docMedia.Document.(*tg.Document)
			return doc
		}
		return nil
	}

	messagesFrom := func(res tg.MessagesMessagesClass) []tg.MessageClass {
		switch list := res.(type) {
		case *tg.MessagesMessages:
			return list.Messages
		case *tg.MessagesMessagesSlice:
			return list.Messages
		case *tg.MessagesChannelMessages:
			return list.Messages
		default:
			return nil
		}
	}

	senderIDFrom := func(m *tg.Message) int64 {
		if m == nil || m.FromID == nil {
			return 0
		}
		switch p := m.FromID.(type) {
		case *tg.PeerUser:
			return p.UserID
		case *tg.PeerChat:
			return -p.ChatID
		case *tg.PeerChannel:
			return -1000000000000 - p.ChannelID
		default:
			return 0
		}
	}

	isAudioDoc := func(doc *tg.Document) bool {
		if doc == nil {
			return false
		}
		var fileName string
		for _, attr := range doc.Attributes {
			switch a := attr.(type) {
			case *tg.DocumentAttributeAudio:
				return true
			case *tg.DocumentAttributeFilename:
				fileName = strings.ToLower(strings.TrimSpace(a.FileName))
			}
		}
		mime := strings.ToLower(strings.TrimSpace(doc.MimeType))
		isAudioExt := strings.HasSuffix(fileName, ".flac") || strings.HasSuffix(fileName, ".mp3") ||
			strings.HasSuffix(fileName, ".wav") || strings.HasSuffix(fileName, ".wave") ||
			strings.HasSuffix(fileName, ".m4a") || strings.HasSuffix(fileName, ".alac") ||
			strings.HasSuffix(fileName, ".aac") || strings.HasSuffix(fileName, ".ogg") ||
			strings.HasSuffix(fileName, ".opus") || strings.HasSuffix(fileName, ".aif") ||
			strings.HasSuffix(fileName, ".aiff") || strings.HasSuffix(fileName, ".wma")
		return strings.HasPrefix(mime, "audio/") || isAudioExt ||
			mime == "application/x-flac" || mime == "application/flac" || mime == "application/ogg"
	}

	var targetDoc *tg.Document
	if msg != nil {
		targetDoc = docFromMessage(msg)
	}

	if targetDoc == nil && msg != nil && msg.ReplyTo != nil {
		if header, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && header.ReplyToMsgID != 0 {
			if h.tgService != nil {
				if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
					res, err := w.API.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: header.ReplyToMsgID}})
					if err == nil {
						for _, m := range messagesFrom(res) {
							if replied, ok := m.(*tg.Message); ok {
								if doc := docFromMessage(replied); doc != nil {
									targetDoc = doc
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if targetDoc == nil && msg != nil && h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			if peer := extractInputPeer(msg.PeerID, e, w); peer != nil {
				res, err := w.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
					Peer:     peer,
					OffsetID: msg.ID,
					Limit:    30,
				})
				if err == nil {
					commandSender := senderIDFrom(msg)
					var fallbackDoc *tg.Document
					for _, m := range messagesFrom(res) {
						older, ok := m.(*tg.Message)
						if !ok {
							continue
						}
						if msg.ID != 0 && older.ID >= msg.ID {
							continue
						}
						if candidateSender := senderIDFrom(older); commandSender != 0 && candidateSender != 0 && candidateSender != commandSender {
							continue
						}
						doc := docFromMessage(older)
						if doc == nil {
							continue
						}
						if fallbackDoc == nil {
							fallbackDoc = doc
						}
						if isAudioDoc(doc) {
							targetDoc = doc
							break
						}
					}
					if targetDoc == nil {
						targetDoc = fallbackDoc
					}
				}
			}
		}
	}

	if targetDoc == nil {
		_ = h.replyHTML(ctx, e, upd, "No recent media found. Send an audio/media file and then /mi, or reply to it with /mi.", nil)
		return
	}

	sentMessageID := func(update tg.UpdatesClass) int {
		fromMessage := func(m tg.MessageClass) int {
			if full, ok := m.(*tg.Message); ok {
				return full.ID
			}
			return 0
		}
		fromUpdates := func(items []tg.UpdateClass) int {
			for _, item := range items {
				switch u := item.(type) {
				case *tg.UpdateNewMessage:
					if id := fromMessage(u.Message); id != 0 {
						return id
					}
				case *tg.UpdateNewChannelMessage:
					if id := fromMessage(u.Message); id != 0 {
						return id
					}
				}
			}
			return 0
		}

		switch u := update.(type) {
		case *tg.UpdateShortMessage:
			return u.ID
		case *tg.UpdateShortSentMessage:
			return u.ID
		case *tg.UpdateShort:
			if nested, ok := u.Update.(*tg.UpdateNewMessage); ok {
				return fromMessage(nested.Message)
			}
			if nested, ok := u.Update.(*tg.UpdateNewChannelMessage); ok {
				return fromMessage(nested.Message)
			}
		case *tg.Updates:
			return fromUpdates(u.Updates)
		case *tg.UpdatesCombined:
			return fromUpdates(u.Updates)
		}
		return 0
	}

	waitMessageID := 0
	if h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			if updates, err := message.NewSender(w.API).Reply(e, upd).Text(ctx, "⏳ Please wait, getting media information..."); err == nil {
				waitMessageID = sentMessageID(updates)
			}
		}
	}

	var title, artist, fileName string
	var duration int
	for _, attr := range targetDoc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeAudio:
			title = a.Title
			artist = a.Performer
			duration = a.Duration
		case *tg.DocumentAttributeFilename:
			fileName = a.FileName
		}
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = "audio"
	}

	analysisCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	parseFloat := func(raw string) (float64, bool) {
		s := strings.TrimSpace(strings.ReplaceAll(raw, ",", "."))
		if s == "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	}
	normalizeKey := func(raw string) string {
		var key strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				key.WriteRune(r)
			}
		}
		return key.String()
	}
	fmtRate := func(v float64) string {
		if v <= 0 {
			return ""
		}
		if v >= 1_000_000 {
			return fmt.Sprintf("%.2fm", v/1_000_000)
		}
		if v >= 1_000 {
			return fmt.Sprintf("%.1fk", v/1_000)
		}
		return fmt.Sprintf("%.0f", v)
	}
	fmtSize := func(v float64) string {
		if v >= 1024*1024 {
			return fmt.Sprintf("%.1fm", v/(1024*1024))
		}
		if v >= 1024 {
			return fmt.Sprintf("%.1fk", v/1024)
		}
		return fmt.Sprintf("%.0f", v)
	}

	audioPath := ""
	defer func() {
		if audioPath != "" {
			_ = os.Remove(audioPath)
		}
	}()
	if h.tgService != nil {
		ext := strings.ToLower(filepath.Ext(fileName))
		if ext == "" {
			mime := strings.ToLower(targetDoc.MimeType)
			switch {
			case strings.Contains(mime, "flac"):
				ext = ".flac"
			case strings.Contains(mime, "mpeg") || strings.Contains(mime, "mp3"):
				ext = ".mp3"
			case strings.Contains(mime, "mp4") || strings.Contains(mime, "m4a"):
				ext = ".m4a"
			default:
				ext = ".bin"
			}
		}
		location := &tg.InputDocumentFileLocation{
			ID:            targetDoc.ID,
			AccessHash:    targetDoc.AccessHash,
			FileReference: targetDoc.FileReference,
		}
		if tmp, err := os.CreateTemp("", "streamgo_mi_*"+ext); err == nil {
			audioPath = tmp.Name()
			dlErr := h.tgService.DownloadPartial(analysisCtx, location, 0, tmp)
			_ = tmp.Close()
			if dlErr != nil {
				_ = os.Remove(audioPath)
				audioPath = ""
			}
		}
	}

	info := map[string]string{}
	soxStats := map[string]float64{}
	tags := map[string]string{}
	if audioPath != "" {
		parseComment := func(line string) {
			line = strings.Trim(strings.TrimSpace(line), "'\"")
			if line == "" {
				return
			}
			if idx := strings.Index(line, "="); idx > 0 {
				if key, value := strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]); key != "" && value != "" {
					tags[normalizeKey(key)] = value
					return
				}
			}
			if idx := strings.Index(line, ":"); idx > 0 {
				if key, value := strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]); key != "" && value != "" {
					tags[normalizeKey(key)] = value
					return
				}
			}
			tags["comment"] = line
		}
		knownInfoKeys := map[string]bool{
			"inputfile": true, "channels": true, "samplerate": true, "precision": true,
			"duration": true, "filesize": true, "bitrate": true, "sampleencoding": true,
			"endiantype": true, "reversenibbles": true, "reversebits": true,
			"replaygain": true, "leveladjust": true, "comment": true, "comments": true,
		}
		if out, err := exec.CommandContext(analysisCtx, "sox", "--i", "-a", audioPath).CombinedOutput(); err == nil {
			inComments := false
			for _, raw := range strings.Split(string(out), "\n") {
				line := strings.TrimSpace(raw)
				if line == "" {
					inComments = false
					continue
				}
				if inComments {
					if idx := strings.Index(line, ":"); idx >= 0 && !knownInfoKeys[normalizeKey(line[:idx])] {
						parseComment(line)
						continue
					}
					if strings.Contains(line, "=") {
						parseComment(line)
						continue
					}
					inComments = false
				}
				idx := strings.Index(line, ":")
				if idx < 0 {
					continue
				}
				key := normalizeKey(line[:idx])
				value := strings.TrimSpace(line[idx+1:])
				if key == "" {
					continue
				}
				info[key] = value
				switch key {
				case "comments":
					inComments = true
					if value != "" {
						parseComment(value)
					}
				case "comment":
					parseComment(value)
				}
			}
		}
		if out, err := exec.CommandContext(analysisCtx, "sox", audioPath, "-n", "stat").CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) != 2 {
					continue
				}
				key := normalizeKey(parts[0])
				if key == "" {
					continue
				}
				if _, exists := soxStats[key]; exists {
					continue
				}
				if value, ok := parseFloat(parts[1]); ok {
					soxStats[key] = value
				}
			}
		}
	}

	tagKey := normalizeKey
	tagValue := func(names ...string) string {
		for _, name := range names {
			wanted := tagKey(name)
			if wanted == "" {
				continue
			}
			for key, value := range tags {
				if tagKey(key) == wanted && value != "" {
					return value
				}
			}
		}
		return ""
	}

	lengthSeconds := float64(duration)
	if v, ok := soxStats["lengthseconds"]; ok && v > 0 {
		lengthSeconds = v
	}
	bitRateText := strings.TrimSpace(info["bitrate"])
	if bitRateText == "" && lengthSeconds > 0 {
		bitRateText = fmtRate(float64(targetDoc.Size) * 8 / lengthSeconds)
	}
	fileSizeText := strings.TrimSpace(info["filesize"])
	if fileSizeText == "" {
		fileSizeText = fmtSize(float64(targetDoc.Size))
	}

	metaTitle := tagValue("title")
	metaAlbum := tagValue("album")
	metaDate := tagValue("date", "year")
	metaCopyright := tagValue("copyright")
	metaBarcode := tagValue("barcode")
	metaArtist := tagValue("artist")
	metaAlbumArtist := tagValue("albumartist", "album artist", "album_artist")

	if metaTitle == "" {
		metaTitle = title
	}
	if metaArtist == "" {
		metaArtist = artist
	}

	comments := tagValue("commenthttps", "commenthttpsurl", "comment url", "applemusicurl")
	if comments == "" {
		for key, value := range tags {
			normalizedKey := tagKey(key)
			lowerValue := strings.ToLower(value)
			if strings.Contains(normalizedKey, "comment") &&
				(strings.Contains(normalizedKey, "https") || strings.HasPrefix(lowerValue, "http") || strings.HasPrefix(lowerValue, "//")) {
				comments = value
				break
			}
		}
	}

	bpm := ""
	if rawBPM := tagValue("bpm", "tbpm"); rawBPM != "" {
		if fields := strings.Fields(rawBPM); len(fields) > 0 {
			if value, ok := parseFloat(fields[0]); ok && value > 0 {
				bpm = fmt.Sprintf("%.0f", value)
			}
		}
	}

	var sb strings.Builder
	writeLine := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			sb.WriteString(label + ":\n")
			return
		}
		sb.WriteString(label + ": <b>" + html.EscapeString(value) + "</b>\n")
	}

	writeLine("Filename", fileName)
	durationText := ""
	if lengthSeconds > 0 {
		roundedSeconds := int(math.Round(lengthSeconds))
		durationText = fmt.Sprintf("%02d:%02d seconds", roundedSeconds/60, roundedSeconds%60)
	}
	writeLine("Duration", durationText)
	writeLine("BPM", bpm)

	sb.WriteString("\n")
	writeLine("title", metaTitle)
	writeLine("album", metaAlbum)
	writeLine("date", metaDate)
	writeLine("copyright", metaCopyright)
	writeLine("barcode", metaBarcode)
	writeLine("artist", metaArtist)
	writeLine("albumartist", metaAlbumArtist)

	sb.WriteString("\n")
	writeLine("bitRate", bitRateText)
	writeLine("channels", strings.TrimSpace(info["channels"]))
	writeLine("comments", comments)
	writeLine("fileSize", fileSizeText)
	writeLine("lengthSeconds", fmt.Sprintf("%.6f", lengthSeconds))

	putSoxStat := func(label, key string, digits int) {
		valueText := ""
		if value, ok := soxStats[key]; ok {
			valueText = fmt.Sprintf("%.*f", digits, value)
		}
		writeLine(label, valueText)
	}

	putSoxStat("maximumAmplitude", "maximumamplitude", 6)
	putSoxStat("maximumDelta", "maximumdelta", 6)
	putSoxStat("meanAmplitude", "meanamplitude", 6)
	putSoxStat("meanDelta", "meandelta", 6)
	putSoxStat("meanNorm", "meannorm", 6)
	putSoxStat("midlineAmplitude", "midlineamplitude", 6)
	putSoxStat("minimumAmplitude", "minimumamplitude", 6)
	putSoxStat("minimumDelta", "minimumdelta", 6)
	putSoxStat("rmsAmplitude", "rmsamplitude", 6)
	putSoxStat("rmsDelta", "rmsdelta", 6)
	putSoxStat("roughFrequency", "roughfrequency", 0)

	writeLine("precision", strings.TrimSpace(info["precision"]))
	writeLine("sampleEncoding", strings.TrimSpace(info["sampleencoding"]))
	writeLine("sampleRate", strings.TrimSpace(info["samplerate"]))
	putSoxStat("samplesRead", "samplesread", 0)
	putSoxStat("scaledBy", "scaledby", 1)
	putSoxStat("volumeAdjustment", "volumeadjustment", 3)

	_ = h.replyHTML(ctx, e, upd, sb.String(), nil)

	if waitMessageID != 0 && h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer deleteCancel()
			_, _ = w.API.MessagesDeleteMessages(deleteCtx, &tg.MessagesDeleteMessagesRequest{
				ID:     []int{waitMessageID},
				Revoke: true,
			})
		}
	}
}

// handleSpectrogram generates and sends a spectrogram for an attached, replied, or recently sent audio file.
func (h *Handler) handleSpectrogram(ctx context.Context, e tg.Entities, upd message.AnswerableMessageUpdate, msg *tg.Message) {
	docFromMessage := func(m *tg.Message) *tg.Document {
		if m == nil || m.Media == nil {
			return nil
		}
		if docMedia, ok := m.Media.(*tg.MessageMediaDocument); ok && docMedia.Document != nil {
			doc, _ := docMedia.Document.(*tg.Document)
			return doc
		}
		return nil
	}

	messagesFrom := func(res tg.MessagesMessagesClass) []tg.MessageClass {
		switch list := res.(type) {
		case *tg.MessagesMessages:
			return list.Messages
		case *tg.MessagesMessagesSlice:
			return list.Messages
		case *tg.MessagesChannelMessages:
			return list.Messages
		default:
			return nil
		}
	}

	senderIDFrom := func(m *tg.Message) int64 {
		if m == nil || m.FromID == nil {
			return 0
		}
		switch p := m.FromID.(type) {
		case *tg.PeerUser:
			return p.UserID
		case *tg.PeerChat:
			return -p.ChatID
		case *tg.PeerChannel:
			return -1000000000000 - p.ChannelID
		default:
			return 0
		}
	}

	isAudioDoc := func(doc *tg.Document) bool {
		if doc == nil {
			return false
		}
		var fileName string
		for _, attr := range doc.Attributes {
			switch a := attr.(type) {
			case *tg.DocumentAttributeAudio:
				return true
			case *tg.DocumentAttributeFilename:
				fileName = strings.ToLower(strings.TrimSpace(a.FileName))
			}
		}
		mime := strings.ToLower(strings.TrimSpace(doc.MimeType))
		isAudioExt := strings.HasSuffix(fileName, ".flac") || strings.HasSuffix(fileName, ".mp3") ||
			strings.HasSuffix(fileName, ".wav") || strings.HasSuffix(fileName, ".wave") ||
			strings.HasSuffix(fileName, ".m4a") || strings.HasSuffix(fileName, ".alac") ||
			strings.HasSuffix(fileName, ".aac") || strings.HasSuffix(fileName, ".ogg") ||
			strings.HasSuffix(fileName, ".opus") || strings.HasSuffix(fileName, ".aif") ||
			strings.HasSuffix(fileName, ".aiff") || strings.HasSuffix(fileName, ".wma")
		return strings.HasPrefix(mime, "audio/") || isAudioExt ||
			mime == "application/x-flac" || mime == "application/flac" || mime == "application/ogg"
	}

	var targetDoc *tg.Document
	if msg != nil {
		targetDoc = docFromMessage(msg)
	}

	if targetDoc == nil && msg != nil && msg.ReplyTo != nil {
		if header, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && header.ReplyToMsgID != 0 {
			if h.tgService != nil {
				if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
					res, err := w.API.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: header.ReplyToMsgID}})
					if err == nil {
						for _, m := range messagesFrom(res) {
							if replied, ok := m.(*tg.Message); ok {
								if doc := docFromMessage(replied); doc != nil {
									targetDoc = doc
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if targetDoc == nil && msg != nil && h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			if peer := extractInputPeer(msg.PeerID, e, w); peer != nil {
				res, err := w.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
					Peer:     peer,
					OffsetID: msg.ID,
					Limit:    30,
				})
				if err == nil {
					commandSender := senderIDFrom(msg)
					var fallbackDoc *tg.Document
					for _, m := range messagesFrom(res) {
						older, ok := m.(*tg.Message)
						if !ok {
							continue
						}
						if msg.ID != 0 && older.ID >= msg.ID {
							continue
						}
						if candidateSender := senderIDFrom(older); commandSender != 0 && candidateSender != 0 && candidateSender != commandSender {
							continue
						}
						doc := docFromMessage(older)
						if doc == nil {
							continue
						}
						if fallbackDoc == nil {
							fallbackDoc = doc
						}
						if isAudioDoc(doc) {
							targetDoc = doc
							break
						}
					}
					if targetDoc == nil {
						targetDoc = fallbackDoc
					}
				}
			}
		}
	}

	if targetDoc == nil {
		_ = h.replyHTML(ctx, e, upd, "No recent media found. Send an audio/media file and then /spec, or reply to it with /spec.", nil)
		return
	}

	sentMessageID := func(update tg.UpdatesClass) int {
		fromMessage := func(m tg.MessageClass) int {
			if full, ok := m.(*tg.Message); ok {
				return full.ID
			}
			return 0
		}
		fromUpdates := func(items []tg.UpdateClass) int {
			for _, item := range items {
				switch u := item.(type) {
				case *tg.UpdateNewMessage:
					if id := fromMessage(u.Message); id != 0 {
						return id
					}
				case *tg.UpdateNewChannelMessage:
					if id := fromMessage(u.Message); id != 0 {
						return id
					}
				}
			}
			return 0
		}

		switch u := update.(type) {
		case *tg.UpdateShortMessage:
			return u.ID
		case *tg.UpdateShortSentMessage:
			return u.ID
		case *tg.UpdateShort:
			if nested, ok := u.Update.(*tg.UpdateNewMessage); ok {
				return fromMessage(nested.Message)
			}
			if nested, ok := u.Update.(*tg.UpdateNewChannelMessage); ok {
				return fromMessage(nested.Message)
			}
		case *tg.Updates:
			return fromUpdates(u.Updates)
		case *tg.UpdatesCombined:
			return fromUpdates(u.Updates)
		}
		return 0
	}

	waitMessageID := 0
	if h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			if updates, err := message.NewSender(w.API).Reply(e, upd).Text(ctx, "⏳ Please wait, generating spectrogram..."); err == nil {
				waitMessageID = sentMessageID(updates)
			}
		}
	}

	var fileName string
	for _, attr := range targetDoc.Attributes {
		if a, ok := attr.(*tg.DocumentAttributeFilename); ok {
			fileName = a.FileName
			break
		}
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = "audio"
	}

	analysisCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	audioPath := ""
	spectrogramPath := ""
	spectrogramErrText := ""
	defer func() {
		if audioPath != "" {
			_ = os.Remove(audioPath)
		}
		if spectrogramPath != "" {
			_ = os.Remove(spectrogramPath)
		}
	}()

	if h.tgService != nil {
		ext := strings.ToLower(filepath.Ext(fileName))
		if ext == "" {
			mime := strings.ToLower(targetDoc.MimeType)
			switch {
			case strings.Contains(mime, "flac"):
				ext = ".flac"
			case strings.Contains(mime, "mpeg") || strings.Contains(mime, "mp3"):
				ext = ".mp3"
			case strings.Contains(mime, "mp4") || strings.Contains(mime, "m4a"):
				ext = ".m4a"
			default:
				ext = ".bin"
			}
		}
		location := &tg.InputDocumentFileLocation{
			ID:            targetDoc.ID,
			AccessHash:    targetDoc.AccessHash,
			FileReference: targetDoc.FileReference,
		}
		if tmp, err := os.CreateTemp("", "streamgo_spec_src_*"+ext); err == nil {
			audioPath = tmp.Name()
			dlErr := h.tgService.DownloadPartial(analysisCtx, location, 0, tmp)
			_ = tmp.Close()
			if dlErr != nil {
				_ = os.Remove(audioPath)
				audioPath = ""
			}
		}
	}

	formatSpecError := func(output []byte, err error) string {
		text := strings.TrimSpace(string(output))
		if text == "" && err != nil {
			text = err.Error()
		}
		text = strings.Join(strings.Fields(text), " ")
		if runes := []rune(text); len(runes) > 500 {
			text = string(runes[:500]) + "..."
		}
		return text
	}

	if audioPath == "" {
		_ = h.replyHTML(ctx, e, upd, "Unable to download the audio for the spectrogram.", nil)
	} else if spec, err := os.CreateTemp("", "streamgo_spec_*.png"); err == nil {
		spectrogramPath = spec.Name()
		_ = spec.Close()
		_ = os.Remove(spectrogramPath)
		cmd := exec.CommandContext(analysisCtx, "sox", audioPath, "-n", "spectrogram", "-o", spectrogramPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			_ = os.Remove(spectrogramPath)
			spectrogramPath = ""
			spectrogramErrText = formatSpecError(output, err)
		}
	}

	if audioPath != "" && spectrogramPath == "" {
		soxErrText := spectrogramErrText
		if spec, err := os.CreateTemp("", "streamgo_spec_pipe_*.png"); err == nil {
			spectrogramPath = spec.Name()
			_ = spec.Close()
			_ = os.Remove(spectrogramPath)

			decoder := exec.CommandContext(analysisCtx, "ffmpeg", "-v", "error", "-i", audioPath,
				"-map", "0:a:0", "-vn", "-f", "sox", "-")
			var decoderErr bytes.Buffer
			decoder.Stderr = &decoderErr
			decoderPipe, err := decoder.StdoutPipe()
			if err != nil {
				_ = os.Remove(spectrogramPath)
				spectrogramPath = ""
				if decoderErrText := formatSpecError([]byte(err.Error()), nil); decoderErrText != "" {
					spectrogramErrText = "sox: " + soxErrText + " | ffmpeg decode: " + decoderErrText
				}
			} else if err := decoder.Start(); err != nil {
				_ = os.Remove(spectrogramPath)
				spectrogramPath = ""
				fallbackErrText := formatSpecError(nil, err)
				combinedErrText := "sox: " + soxErrText + " | ffmpeg decode: " + fallbackErrText
				spectrogramErrText = formatSpecError([]byte(combinedErrText), nil)
			} else {
				soxCmd := exec.CommandContext(analysisCtx, "sox", "-p", "-n", "spectrogram", "-o", spectrogramPath)
				soxCmd.Stdin = decoderPipe
				soxOutput, soxErr := soxCmd.CombinedOutput()
				if soxErr != nil {
					_ = decoderPipe.Close()
				}
				decoderErrRun := decoder.Wait()

				if soxErr != nil || decoderErrRun != nil {
					_ = os.Remove(spectrogramPath)
					spectrogramPath = ""
					fallbackErrText := formatSpecError(soxOutput, soxErr)
					if decoderErrText := formatSpecError(decoderErr.Bytes(), decoderErrRun); decoderErrText != "" {
						if fallbackErrText == "" {
							fallbackErrText = "ffmpeg decode: " + decoderErrText
						} else {
							fallbackErrText += " | ffmpeg decode: " + decoderErrText
						}
					}
					combinedErrText := "sox: " + soxErrText + " | fallback: " + fallbackErrText
					spectrogramErrText = formatSpecError([]byte(combinedErrText), nil)
				} else {
					spectrogramErrText = ""
				}
			}
		}
	}

	if spectrogramPath != "" {
		if h.tgService != nil {
			if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
				if uploaded, err := uploader.NewUploader(w.API).FromPath(analysisCtx, spectrogramPath); err == nil {
					specFileName := "spectrogram.png"
					if base := strings.TrimSpace(filepath.Base(fileName)); base != "" && base != "." {
						specFileName = strings.TrimSuffix(base, filepath.Ext(base)) + "_spectrogram.png"
					}
					_, _ = message.NewSender(w.API).Reply(e, upd).Media(analysisCtx,
						message.UploadedDocument(uploaded).
							MIME("image/png").
							Filename(specFileName).
							ForceFile(true))
				} else {
					_ = h.replyHTML(ctx, e, upd, "Unable to upload the spectrogram.", nil)
				}
			}
		}
	} else if audioPath != "" {
		reply := "Unable to generate spectrogram."
		if spectrogramErrText != "" {
			reply += "\n<code>" + html.EscapeString(spectrogramErrText) + "</code>"
		}
		_ = h.replyHTML(ctx, e, upd, reply, nil)
	}

	if waitMessageID != 0 && h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer deleteCancel()
			_, _ = w.API.MessagesDeleteMessages(deleteCtx, &tg.MessagesDeleteMessagesRequest{
				ID:     []int{waitMessageID},
				Revoke: true,
			})
		}
	}
}
