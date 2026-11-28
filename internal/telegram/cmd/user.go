package cmd

import (
	"bytes"
	"context"
	"encoding/json"
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

	type ffprobeData struct {
		Format struct {
			Duration string            `json:"duration"`
			BitRate  string            `json:"bit_rate"`
			Size     string            `json:"size"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			CodecName        string            `json:"codec_name"`
			SampleRate       string            `json:"sample_rate"`
			Channels         int               `json:"channels"`
			BitsPerSample    int               `json:"bits_per_sample"`
			BitsPerRawSample string            `json:"bits_per_raw_sample"`
			Duration         string            `json:"duration"`
			BitRate          string            `json:"bit_rate"`
			Tags             map[string]string `json:"tags"`
		} `json:"streams"`
	}

	audioPath := ""
	defer func() {
		if audioPath != "" {
			_ = os.Remove(audioPath)
		}
	}()
	probeErr := ""
	if h.tgService == nil {
		probeErr = "Telegram service is unavailable."
	} else {
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
		if tmp, err := os.CreateTemp("", "streamgo_mi_*"+ext); err != nil {
			probeErr = err.Error()
		} else {
			audioPath = tmp.Name()
			dlErr := h.tgService.DownloadPartial(analysisCtx, location, 8<<20, tmp)
			_ = tmp.Close()
			if dlErr != nil {
				_ = os.Remove(audioPath)
				audioPath = ""
				probeErr = dlErr.Error()
			}
		}
	}

	var probe ffprobeData
	if audioPath != "" {
		out, err := exec.CommandContext(analysisCtx, "ffprobe", "-v", "quiet", "-print_format", "json",
			"-show_format", "-show_streams", "-show_chapters", audioPath).Output()
		if err != nil {
			probeErr = err.Error()
		} else if err := json.Unmarshal(out, &probe); err != nil {
			probeErr = err.Error()
		}
	}

	tags := map[string]string{}
	for _, stream := range probe.Streams {
		for key, value := range stream.Tags {
			tags[key] = strings.TrimSpace(value)
		}
	}
	for key, value := range probe.Format.Tags {
		tags[key] = strings.TrimSpace(value)
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
	if v, ok := parseFloat(probe.Format.Duration); ok && v > 0 {
		lengthSeconds = v
	} else if len(probe.Streams) > 0 {
		if v, ok := parseFloat(probe.Streams[0].Duration); ok && v > 0 {
			lengthSeconds = v
		}
	}

	bitRateText := ""
	if v, ok := parseFloat(probe.Format.BitRate); ok && v > 0 {
		bitRateText = fmtRate(v)
	} else if len(probe.Streams) > 0 {
		if v, ok := parseFloat(probe.Streams[0].BitRate); ok && v > 0 {
			bitRateText = fmtRate(v)
		}
	}
	if bitRateText == "" && lengthSeconds > 0 {
		bitRateText = fmtRate(float64(targetDoc.Size) * 8 / lengthSeconds)
	}

	codecText := ""
	sampleRateText := ""
	channelsText := ""
	bitDepthText := ""
	if len(probe.Streams) > 0 {
		stream := probe.Streams[0]
		codecText = strings.TrimSpace(stream.CodecName)
		sampleRateText = strings.TrimSpace(stream.SampleRate)
		if stream.Channels > 0 {
			channelsText = strconv.Itoa(stream.Channels)
		}
		if v, ok := parseFloat(stream.BitsPerRawSample); ok && v > 0 {
			bitDepthText = fmt.Sprintf("%d-bit", int(v))
		} else if stream.BitsPerSample > 0 {
			bitDepthText = fmt.Sprintf("%d-bit", stream.BitsPerSample)
		}
	}

	metaTitle := tagValue("title")
	if metaTitle == "" {
		metaTitle = title
	}
	metaArtist := tagValue("artist")
	if metaArtist == "" {
		metaArtist = artist
	}
	metaAlbum := tagValue("album")
	metaAlbumArtist := tagValue("albumartist", "album artist", "album_artist")
	metaDate := tagValue("date", "year")
	metaGenre := tagValue("genre")
	metaTrack := tagValue("track", "tracknumber")
	metaDisc := tagValue("disc", "discnumber")
	metaCopyright := tagValue("copyright")
	metaBarcode := tagValue("barcode")
	comments := tagValue("comment", "comments", "commenthttps", "commenthttpsurl", "applemusicurl")
	if comments == "" {
		for key, value := range tags {
			lowerValue := strings.ToLower(value)
			if strings.Contains(tagKey(key), "comment") &&
				(strings.HasPrefix(lowerValue, "http") || strings.HasPrefix(lowerValue, "//")) {
				comments = value
				break
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

	replyText := ""
	if probeErr != "" {
		replyText = "Unable to read media information."
		if detail := strings.Join(strings.Fields(probeErr), " "); detail != "" {
			replyText += "\n<code>" + html.EscapeString(detail) + "</code>"
		}
	} else {
		durationText := ""
		if lengthSeconds > 0 {
			roundedSeconds := int(math.Round(lengthSeconds))
			durationText = fmt.Sprintf("%02d:%02d seconds", roundedSeconds/60, roundedSeconds%60)
		}
		writeLine("Filename", fileName)
		writeLine("Codec", codecText)
		writeLine("Duration", durationText)
		writeLine("Bitrate", bitRateText)
		writeLine("Sample Rate", sampleRateText)
		writeLine("Channels", channelsText)
		writeLine("Bit Depth", bitDepthText)
		writeLine("File Size", fmtSize(float64(targetDoc.Size)))

		sb.WriteString("\n")
		writeLine("Title", metaTitle)
		writeLine("Artist", metaArtist)
		writeLine("Album", metaAlbum)
		writeLine("Album Artist", metaAlbumArtist)
		writeLine("Date", metaDate)
		writeLine("Genre", metaGenre)
		writeLine("Track", metaTrack)
		writeLine("Disc", metaDisc)
		writeLine("Copyright", metaCopyright)
		writeLine("Barcode", metaBarcode)
		writeLine("Comment", comments)
		replyText = sb.String()
	}

	_ = h.replyHTML(ctx, e, upd, replyText, nil)

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
