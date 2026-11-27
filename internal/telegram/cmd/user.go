package cmd

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
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

	audioPath := ""
	spectrogramPath := ""
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
		if audioPath != "" {
			if spec, err := os.CreateTemp("", "streamgo_spec_*.png"); err == nil {
				spectrogramPath = spec.Name()
				_ = spec.Close()
				_ = os.Remove(spectrogramPath)
				cmd := exec.CommandContext(analysisCtx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
					"-i", audioPath, "-lavfi", "showspectrumpic=s=1280x720:legend=1:mode=combined", spectrogramPath)
				if _, err := cmd.CombinedOutput(); err != nil {
					_ = os.Remove(spectrogramPath)
					spectrogramPath = ""
				}
			}
		}
	}
	defer func() {
		if audioPath != "" {
			_ = os.Remove(audioPath)
		}
		if spectrogramPath != "" {
			_ = os.Remove(spectrogramPath)
		}
	}()

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

	_ = h.replyHTML(ctx, e, upd, sb.String(), nil)

	if spectrogramPath != "" && h.tgService != nil {
		if w := h.tgService.PrimaryWorker(); w != nil && w.API != nil {
			if uploaded, err := uploader.NewUploader(w.API).FromPath(analysisCtx, spectrogramPath); err == nil {
				_, _ = message.NewSender(w.API).Reply(e, upd).UploadedPhoto(analysisCtx, uploaded)
			}
		}
	}
}
