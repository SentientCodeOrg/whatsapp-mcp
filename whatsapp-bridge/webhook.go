package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// webhookMediaTypes returns the media types (image, audio, video, document,
// location) that are downloaded synchronously and forwarded with the webhook,
// from WEBHOOK_MEDIA_TYPES (comma-separated; default "image").
func webhookMediaTypes() map[string]bool {
	v := os.Getenv("WEBHOOK_MEDIA_TYPES")
	if strings.TrimSpace(v) == "" {
		v = "image"
	}
	set := map[string]bool{}
	for _, t := range strings.Split(v, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			set[t] = true
		}
	}
	return set
}

// webhookMediaMaxBytes is the largest file that is base64-encoded into a
// webhook payload (WEBHOOK_MEDIA_MAX_MB, default 16). Larger files are sent
// without bytes and with mediaTooLarge set.
func webhookMediaMaxBytes() int64 {
	mb, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("WEBHOOK_MEDIA_MAX_MB")), 64)
	if err != nil || mb <= 0 {
		mb = 16
	}
	return int64(mb * 1024 * 1024)
}

// webhookMimeType sniffs the downloaded file's type. Where sniffing only finds
// a container (zip for Office files, ogg for voice notes) or nothing, the type
// the sender declared is more precise and is kept.
func webhookMimeType(path, declared string) string {
	sniffed := ""
	if f, err := os.Open(path); err == nil {
		buf := make([]byte, 512)
		if n, _ := f.Read(buf); n > 0 {
			sniffed = http.DetectContentType(buf[:n])
		}
		_ = f.Close()
	}
	switch sniffed {
	case "", "application/octet-stream", "application/zip", "application/ogg":
		if declared != "" {
			return declared
		}
	}
	if sniffed == "" {
		return "application/octet-stream"
	}
	return sniffed
}

// webhookClient is used for all outbound webhook POSTs. The 30-second timeout
// prevents a slow or unreachable endpoint from blocking message handling
// indefinitely. Redirects are never followed: WEBHOOK_URL is a single
// operator-configured endpoint, not a browsable URL, and following a 3xx
// would forward X-Bridge-Token to whatever host the redirect names — Go only
// strips Authorization/Cookie on cross-origin redirects, not custom headers.
var webhookClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// webhookAuthToken is the shared bridge token attached as an
// "X-Bridge-Token" header to every outbound webhook POST. It is populated
// once at startup from loadOrCreateBridgeToken() (see auth.go and main.go) —
// the same token the bridge already requires on inbound /api/* requests.
// When empty (no token configured yet) the header is omitted so deployments
// that predate the token rollout keep working. The receiving hub enforces
// this token on its inbound webhook route once its own WHATSAPP_BRIDGE_TOKEN
// is set to the matching value (autohub PR #898), which accepts the token via
// this header or "Authorization: Bearer". A dedicated header is used here
// (rather than Authorization) so it never collides with a receiver's own
// Authorization-based auth — e.g. HTTP Basic auth that net/http derives
// automatically from credentials embedded in WEBHOOK_URL.
var webhookAuthToken string

// defaultWebhookURL is used when WEBHOOK_URL is not set. It is a var (not a
// const) so tests can point it at a local test server. It must never receive
// the bridge token: unlike an operator-configured WEBHOOK_URL, nothing has
// vetted this address, so any other local process that happens to bind this
// port could otherwise capture the token just by being reachable.
var defaultWebhookURL = "http://localhost:8769/whatsapp/webhook"

// WebhookPayload represents the data sent to the webhook
type WebhookPayload struct {
	EventType       string   `json:"eventType,omitempty"`
	Sender          string   `json:"sender"`
	Content         string   `json:"content"`
	ChatJID         string   `json:"chatJID"`
	IsFromMe        bool     `json:"isFromMe"`
	QuotedMessageId string   `json:"quotedMessageId,omitempty"`
	QuotedSender    string   `json:"quotedSender,omitempty"`
	QuotedContent   string   `json:"quotedContent,omitempty"`
	QuotedIsFromMe  *bool    `json:"quotedIsFromMe,omitempty"`
	MentionedJIDs   []string `json:"mentionedJids,omitempty"`
	// Media fields - populated when the message contains an image attachment
	MessageID     string `json:"messageId,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	MimeType      string `json:"mimeType,omitempty"`
	MediaFilename string `json:"mediaFilename,omitempty"`
	MediaBase64   string `json:"mediaBase64,omitempty"`
	MediaTooLarge bool   `json:"mediaTooLarge,omitempty"`
	PTT           bool   `json:"ptt,omitempty"`
	// Location fields - populated when MediaType is "location".
	Latitude        *float64 `json:"latitude,omitempty"`
	Longitude       *float64 `json:"longitude,omitempty"`
	LocationName    *string  `json:"locationName,omitempty"`
	LocationAddress *string  `json:"locationAddress,omitempty"`
	// Receipt fields - populated when EventType is "receipt".
	ReceiptType string   `json:"receiptType,omitempty"`
	MessageIDs  []string `json:"messageIds,omitempty"`
	Timestamp   string   `json:"timestamp,omitempty"`
	// Reaction fields - populated when EventType is "reaction".
	ReactionToMessageID string  `json:"reactionToMessageId,omitempty"`
	ReactionEmoji       *string `json:"reactionEmoji,omitempty"`
	ReactionRemoved     *bool   `json:"reactionRemoved,omitempty"`
}

// webhooksEnabled reports whether webhook processing is enabled. Keep this
// separate from sendWebhookPayload so media callers can avoid their webhook-only
// file work when delivery is disabled.
func webhooksEnabled() bool {
	return getEnvBool("WEBHOOK_ENABLED", true)
}

// sendWebhookPayload marshals and POSTs a WebhookPayload to the configured webhook URL.
func sendWebhookPayload(payload WebhookPayload) {
	// WEBHOOK_ENABLED=false turns outbound webhooks off entirely. An empty
	// WEBHOOK_URL cannot serve that purpose: os.Getenv cannot tell "unset"
	// from "explicitly empty", and empty deliberately falls back to
	// defaultWebhookURL below. Deployments with no webhook consumer would
	// otherwise POST to that default for every message and log a connection
	// refused error each time.
	if !webhooksEnabled() {
		return
	}

	webhookURL := os.Getenv("WEBHOOK_URL")
	explicitlyConfigured := webhookURL != ""
	if !explicitlyConfigured {
		webhookURL = defaultWebhookURL
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error marshaling webhook payload: %v\n", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Error building webhook request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Authenticate to the hub's fail-closed inbound webhook route with the
	// shared bridge token, via a dedicated header so it can never clobber a
	// receiver's own Authorization-based auth (see webhookAuthToken doc
	// comment above). Only attach it when BOTH a token is configured AND
	// WEBHOOK_URL was explicitly set by the operator — the bridge token also
	// authorizes /api/* calls like sending messages, and the implicit local
	// default is not a destination anyone vetted, so it must never receive it.
	if webhookAuthToken != "" && explicitlyConfigured {
		req.Header.Set("X-Bridge-Token", webhookAuthToken)
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		fmt.Printf("Error sending webhook: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 200 {
		fmt.Printf("✓ Webhook sent for message from %s\n", payload.Sender)
	} else {
		fmt.Printf("⚠ Webhook failed with status %d\n", resp.StatusCode)
	}
}

// SendWebhook sends a text-only message to the webhook endpoint. New callers
// should use SendWebhookWithMessageID so receiver-side idempotency can identify
// repeated WhatsApp events; this wrapper remains for compatibility.
func SendWebhook(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string) {
	SendWebhookWithMessageID(sender, content, chatJID, isFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, "")
}

// SendWebhookWithMessageID sends a text-only message and preserves the native
// WhatsApp message ID in the payload for downstream idempotency.
func SendWebhookWithMessageID(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string, messageID string) {
	sendWebhookPayload(WebhookPayload{
		Sender:          sender,
		Content:         content,
		ChatJID:         chatJID,
		IsFromMe:        isFromMe,
		QuotedMessageId: quotedMessageId,
		QuotedSender:    quotedSender,
		QuotedContent:   quotedContent,
		QuotedIsFromMe:  quotedIsFromMe,
		MentionedJIDs:   mentionedJIDs,
		MessageID:       messageID,
	})
}

// SendWebhookWithMedia sends a message to the webhook endpoint including base64-encoded
// media read from localPath. If localPath is empty or unreadable the webhook is
// still sent – just without the MediaBase64 field so the text caption is not lost.
// tooLarge (or a file over webhookMediaMaxBytes) sends mediaTooLarge instead of bytes.
func SendWebhookWithMedia(
	sender, content, chatJID string,
	isFromMe bool,
	quotedMessageId, quotedSender, quotedContent string,
	quotedIsFromMe *bool, mentionedJIDs []string,
	messageID, mediaType, mimeType, mediaFilename, localPath string,
	ptt, tooLarge bool,
) {
	if !webhooksEnabled() {
		return
	}

	var mediaBase64 string
	if localPath != "" {
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			fmt.Printf("⚠ Could not stat media file for base64 encoding: %v\n", statErr)
		} else if info.Size() > webhookMediaMaxBytes() {
			fmt.Printf("⚠ Media file too large for base64 encoding (%d bytes), skipping MediaBase64\n", info.Size())
			tooLarge = true
		} else if data, err := os.ReadFile(localPath); err == nil {
			mediaBase64 = base64.StdEncoding.EncodeToString(data)
		} else {
			fmt.Printf("⚠ Could not read media file for base64 encoding: %v\n", err)
		}
	}

	sendWebhookPayload(WebhookPayload{
		Sender:          sender,
		Content:         content,
		ChatJID:         chatJID,
		IsFromMe:        isFromMe,
		QuotedMessageId: quotedMessageId,
		QuotedSender:    quotedSender,
		QuotedContent:   quotedContent,
		QuotedIsFromMe:  quotedIsFromMe,
		MentionedJIDs:   mentionedJIDs,
		MessageID:       messageID,
		MediaType:       mediaType,
		MimeType:        mimeType,
		MediaFilename:   mediaFilename,
		MediaBase64:     mediaBase64,
		MediaTooLarge:   tooLarge,
		PTT:             ptt,
	})
}

// SendReceiptWebhook tells the webhook that messages we sent in a 1:1 chat
// were delivered to or read by the other side (WEBHOOK_RECEIPTS=true only).
func SendReceiptWebhook(reader, chatJID, receiptType string, messageIDs []string, at time.Time) {
	sendWebhookPayload(WebhookPayload{
		EventType:   "receipt",
		Sender:      reader,
		ChatJID:     chatJID,
		ReceiptType: receiptType,
		MessageIDs:  messageIDs,
		Timestamp:   at.UTC().Format(time.RFC3339),
	})
}

// declaredMimeType is the MIME type the sender declared for a message's media.
func declaredMimeType(msg *waProto.Message) string {
	msg = unwrapMessageContent(msg)
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetMimetype()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetMimetype()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetMimetype()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetMimetype()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetMimetype()
	}
	return ""
}

// liveLocationWindow is how long a live location share from one sender in one
// chat is treated as the same share: only its first fix is forwarded. WhatsApp
// caps a live share at 8 hours.
const liveLocationWindow = 8 * time.Hour

var (
	liveLocationMu   sync.Mutex
	liveLocationSeen = map[string]time.Time{}
)

// firstLiveFix reports whether a live location from sender in chat starts a
// new share, recording it if so.
// ponytail: per chat+sender time window, not per share id (whatsmeow exposes none).
func firstLiveFix(chatJID, sender string, now time.Time) bool {
	liveLocationMu.Lock()
	defer liveLocationMu.Unlock()
	key := chatJID + "|" + sender
	if last, ok := liveLocationSeen[key]; ok && now.Sub(last) < liveLocationWindow {
		return false
	}
	for k, t := range liveLocationSeen {
		if now.Sub(t) >= liveLocationWindow {
			delete(liveLocationSeen, k)
		}
	}
	liveLocationSeen[key] = now
	return true
}

// forwardLocation sends a location pin (or a live location's first fix) to
// the webhook as mediaType "location". Messages without a location are ignored.
func forwardLocation(msg *waProto.Message, sender, chatJID string, isFromMe bool, messageID, quotedMessageId, quotedSender, quotedContent string, mentionedJIDs []string) {
	msg = unwrapMessageContent(msg)
	var lat, lng float64
	var name, address, comment string
	if loc := msg.GetLocationMessage(); loc != nil {
		lat, lng = loc.GetDegreesLatitude(), loc.GetDegreesLongitude()
		name, address, comment = loc.GetName(), loc.GetAddress(), loc.GetComment()
	} else if live := msg.GetLiveLocationMessage(); live != nil {
		if !firstLiveFix(chatJID, sender, time.Now()) {
			return
		}
		lat, lng, comment = live.GetDegreesLatitude(), live.GetDegreesLongitude(), live.GetCaption()
	} else {
		return
	}
	sendWebhookPayload(WebhookPayload{
		Sender:          sender,
		Content:         comment,
		ChatJID:         chatJID,
		IsFromMe:        isFromMe,
		QuotedMessageId: quotedMessageId,
		QuotedSender:    quotedSender,
		QuotedContent:   quotedContent,
		MentionedJIDs:   mentionedJIDs,
		MessageID:       messageID,
		MediaType:       "location",
		Latitude:        &lat,
		Longitude:       &lng,
		LocationName:    &name,
		LocationAddress: &address,
	})
}

// SendReactionWebhook sends a typed reaction event to the webhook endpoint.
func SendReactionWebhook(sender, chatJID string, isFromMe bool, messageID, reactionToMessageID, emoji string) {
	removed := emoji == ""
	sendWebhookPayload(WebhookPayload{
		EventType:           "reaction",
		Sender:              sender,
		Content:             emoji,
		ChatJID:             chatJID,
		IsFromMe:            isFromMe,
		MessageID:           messageID,
		MediaType:           "reaction",
		ReactionToMessageID: reactionToMessageID,
		ReactionEmoji:       &emoji,
		ReactionRemoved:     &removed,
	})
}

// In main.go, handleMessage forwards webhooks for messages with text content.
// It will forward self-sent messages when the env var FORWARD_SELF=true.
