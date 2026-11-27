package cmd

import (
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

	analysisCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
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

	var probe ffprobeData
	stats := map[string]float64{}
	if audioPath != "" {
		if out, err := exec.CommandContext(analysisCtx, "ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", audioPath).Output(); err == nil {
			_ = json.Unmarshal(out, &probe)
		}

		out, _ := exec.CommandContext(analysisCtx, "ffmpeg", "-hide_banner", "-nostats", "-i", audioPath,
			"-af", "astats=metadata=1:reset=0,ametadata=mode=print", "-f", "null", "-").CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			if !strings.Contains(key, "lavfi.astats") {
				continue
			}
			if value, ok := parseFloat(parts[1]); ok {
				stats[key] = value
			}
		}
	}

	stat := func(scope, suffix string) (float64, bool) {
		for key, value := range stats {
			if strings.Contains(key, "."+scope+".") && strings.HasSuffix(key, "."+suffix) {
				return value, true
			}
		}
		return 0, false
	}

	bitRate := float64(0)
	if v, ok := parseFloat(probe.Format.BitRate); ok {
		bitRate = v
	}
	if bitRate == 0 && len(probe.Streams) > 0 {
		if v, ok := parseFloat(probe.Streams[0].BitRate); ok {
			bitRate = v
		}
	}

	fileSize := float64(targetDoc.Size)
	if v, ok := parseFloat(probe.Format.Size); ok && v > 0 {
		fileSize = v
	}

	lengthSeconds := float64(duration)
	if v, ok := parseFloat(probe.Format.Duration); ok && v > 0 {
		lengthSeconds = v
	} else if len(probe.Streams) > 0 {
		if v, ok := parseFloat(probe.Streams[0].Duration); ok && v > 0 {
			lengthSeconds = v
		}
	}

	channels := 0
	bits := 0
	codec := ""
	sampleRate := float64(0)
	comments := ""
	if len(probe.Streams) > 0 {
		s := probe.Streams[0]
		channels = s.Channels
		codec = s.CodecName
		if v, ok := parseFloat(s.SampleRate); ok {
			sampleRate = v
		}
		bits = s.BitsPerSample
		if v, ok := parseFloat(s.BitsPerRawSample); ok && v > 0 {
			bits = int(v)
		}
		if s.Tags != nil {
			comments = strings.TrimSpace(s.Tags["comment"])
		}
	}
	if probe.Format.Tags != nil {
		if v := strings.TrimSpace(probe.Format.Tags["comment"]); v != "" {
			comments = v
		}
	}

	sizeMB := float64(targetDoc.Size) / (1024 * 1024)
	var sb strings.Builder
	sb.WriteString("<b>Media Information:</b>\n\n")
	if title != "" {
		sb.WriteString(fmt.Sprintf("• <b>Title:</b> %s\n", html.EscapeString(title)))
	}
	if artist != "" {
		sb.WriteString(fmt.Sprintf("• <b>Artist:</b> %s\n", html.EscapeString(artist)))
	}
	if fileName != "" {
		sb.WriteString(fmt.Sprintf("• <b>File:</b> <code>%s</code>\n", html.EscapeString(fileName)))
	}
	sb.WriteString(fmt.Sprintf("• <b>MIME:</b> <code>%s</code>\n", html.EscapeString(targetDoc.MimeType)))
	sb.WriteString(fmt.Sprintf("• <b>Size:</b> %.2f MB\n", sizeMB))
	if duration > 0 {
		sb.WriteString(fmt.Sprintf("• <b>Duration:</b> %s\n", formatSeconds(int32(duration))))
	}

	if audioPath != "" {
		sb.WriteString("\n")
		if rate := fmtRate(bitRate); rate != "" {
			sb.WriteString(fmt.Sprintf("bitRate: %s\n", rate))
		}
		if channels > 0 {
			sb.WriteString(fmt.Sprintf("channels: %d\n", channels))
		}
		sb.WriteString(fmt.Sprintf("comments: %s\n", html.EscapeString(comments)))
		sb.WriteString(fmt.Sprintf("fileSize: %s\n", fmtSize(fileSize)))
		sb.WriteString(fmt.Sprintf("lengthSeconds: %.6f\n", lengthSeconds))

		putStat := func(label, scope, suffix string, digits int) {
			if value, ok := stat(scope, suffix); ok {
				sb.WriteString(fmt.Sprintf("%s: %.*f\n", label, digits, value))
			}
		}
		putStat("maximumAmplitude", "Overall", "Max_level", 6)
		putStat("maximumDelta", "Difference", "Max_level", 6)
		putStat("meanAmplitude", "Overall", "Mean_level", 6)
		putStat("meanDelta", "Difference", "Mean_level", 6)
		putStat("meanNorm", "Overall", "Flat_factor", 6)
		putStat("midlineAmplitude", "Overall", "DC_offset", 6)
		putStat("minimumAmplitude", "Overall", "Min_level", 6)
		putStat("minimumDelta", "Difference", "Min_level", 6)
		putStat("rmsAmplitude", "Overall", "RMS_level", 6)
		putStat("rmsDelta", "Difference", "RMS_level", 6)
		putStat("roughFrequency", "Overall", "Zero_crossings_rate", 0)

		if bits > 0 {
			sb.WriteString(fmt.Sprintf("precision: %d-bit\n", bits))
		}
		if codec != "" {
			if bits > 0 {
				sb.WriteString(fmt.Sprintf("sampleEncoding: %d-bit %s\n", bits, html.EscapeString(codec)))
			} else {
				sb.WriteString(fmt.Sprintf("sampleEncoding: %s\n", html.EscapeString(codec)))
			}
		}
		if sampleRate > 0 {
			sb.WriteString(fmt.Sprintf("sampleRate: %.0f\n", sampleRate))
		}
		putStat("samplesRead", "Overall", "Number_of_samples", 0)
		if v, ok := stat("Overall", "Scaled_by"); ok {
			sb.WriteString(fmt.Sprintf("scaledBy: %.1f\n", v))
		}
		if v, ok := stat("Overall", "Volume_adjustment"); ok {
			sb.WriteString(fmt.Sprintf("volumeAdjustment: %.3f\n", v))
		}
	} else {
		sb.WriteString("\nDetailed statistics unavailable.\n")
	}

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

	analysisCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	audioPath := ""
	spectrogramPath := ""
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

	if audioPath == "" {
		_ = h.replyHTML(ctx, e, upd, "Unable to download the audio for the spectrogram.", nil)
	} else {
		nyquist := 24000
		if out, err := exec.CommandContext(analysisCtx, "ffprobe", "-v", "error", "-select_streams", "a:0",
			"-show_entries", "stream=sample_rate", "-of", "csv=p=0", audioPath).Output(); err == nil {
			sampleRate := 0
			if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &sampleRate); err == nil && sampleRate > 1 {
				nyquist = sampleRate / 2
			}
		}

		const fitHeight = 720
		steps := []int{500, 1000, 2000, 4000, 5000, 10000, 20000, 50000, 100000}
		height := 0
		for _, stepHz := range steps {
			candidate := (nyquist*40+stepHz/2)/stepHz + 1
			if candidate <= fitHeight {
				height = candidate
				break
			}
		}
		if height == 0 {
			height = fitHeight
		}
		filter := fmt.Sprintf("showspectrumpic=s=1280x%d:legend=1:mode=combined:fscale=lin:scale=log:start=0:stop=%d", height, nyquist)

		if spec, err := os.CreateTemp("", "streamgo_spec_*.png"); err == nil {
			spectrogramPath = spec.Name()
			_ = spec.Close()
			_ = os.Remove(spectrogramPath)
			cmd := exec.CommandContext(analysisCtx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
				"-i", audioPath, "-lavfi", filter, spectrogramPath)
			if _, err := cmd.CombinedOutput(); err != nil {
				_ = os.Remove(spectrogramPath)
				spectrogramPath = ""
			}
		}
	}

	if spectrogramPath != "" {
		if h.tgService != nil {
			if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
				if uploaded, err := uploader.NewUploader(w.API).FromPath(analysisCtx, spectrogramPath); err == nil {
					_, _ = message.NewSender(w.API).Reply(e, upd).UploadedPhoto(analysisCtx, uploaded)
				} else {
					_ = h.replyHTML(ctx, e, upd, "Unable to upload the spectrogram.", nil)
				}
			}
		}
	} else if audioPath != "" {
		_ = h.replyHTML(ctx, e, upd, "Unable to generate spectrogram.", nil)
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
