package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func messageWith(content *waProto.Message, id string) *events.Message {
	msg := buildImageMessage(phonePN, phonePN, false, "")
	msg.Info.ID = id
	msg.Message = content
	return msg
}

func TestHandleMessage_WebhookMediaTypes(t *testing.T) {
	oggBytes := []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00")
	voiceNote := &waProto.Message{AudioMessage: &waProto.AudioMessage{
		PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus"),
		URL: proto.String("https://example.invalid/a"), MediaKey: []byte("k"), FileLength: proto.Uint64(uint64(len(oggBytes))),
	}}
	bigDocument := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
		FileName: proto.String("verbale.pdf"), Caption: proto.String("il verbale"), Mimetype: proto.String("application/pdf"),
		URL: proto.String("https://example.invalid/d"), MediaKey: []byte("k"), FileLength: proto.Uint64(2 * 1024 * 1024),
	}}

	for _, tc := range []struct {
		name, types, maxMB string
		msg                *waProto.Message
		wantWebhook        bool
		check              func(t *testing.T, p WebhookPayload, calls *mediaDownloadCalls)
	}{
		{name: "default leaves an uncaptioned voice note out", msg: voiceNote},
		{name: "listed voice note is audio with ptt and bytes", types: "image,audio", msg: voiceNote, wantWebhook: true,
			check: func(t *testing.T, p WebhookPayload, calls *mediaDownloadCalls) {
				if p.MediaType != "audio" || !p.PTT || p.MediaTooLarge {
					t.Errorf("mediaType/ptt/tooLarge = %q/%v/%v", p.MediaType, p.PTT, p.MediaTooLarge)
				}
				if p.MediaBase64 != base64.StdEncoding.EncodeToString(oggBytes) {
					t.Error("voice note bytes missing")
				}
				// sniffing finds only the ogg container: the declared codec is kept
				if p.MimeType != "audio/ogg; codecs=opus" {
					t.Errorf("mimeType = %q", p.MimeType)
				}
				if calls.downloads != 1 || calls.scheduled != 0 {
					t.Errorf("downloads/scheduled = %d/%d, want synchronous", calls.downloads, calls.scheduled)
				}
			}},
		{name: "default sends a captioned document as text only", msg: bigDocument, wantWebhook: true,
			check: func(t *testing.T, p WebhookPayload, _ *mediaDownloadCalls) {
				if p.MediaType != "" || p.MediaBase64 != "" || p.Content != "il verbale" {
					t.Errorf("default document webhook changed: %+v", p)
				}
			}},
		{name: "document over the limit is mediaTooLarge without waiting", types: "document", maxMB: "1", msg: bigDocument, wantWebhook: true,
			check: func(t *testing.T, p WebhookPayload, calls *mediaDownloadCalls) {
				if p.MediaType != "document" || p.MediaFilename != "verbale.pdf" || p.Content != "il verbale" {
					t.Errorf("document metadata = %q/%q/%q", p.MediaType, p.MediaFilename, p.Content)
				}
				if !p.MediaTooLarge || p.MediaBase64 != "" {
					t.Error("oversized document must carry mediaTooLarge and no bytes")
				}
				if calls.scheduled != 1 {
					t.Errorf("scheduled = %d, want the async cache download", calls.scheduled)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_ENABLED", "true")
			t.Setenv("WHATSAPP_AUTO_DOWNLOAD_MEDIA", "true")
			t.Setenv("WEBHOOK_MEDIA_TYPES", tc.types)
			t.Setenv("WEBHOOK_MEDIA_MAX_MB", tc.maxMB)
			srv, webhookCh := captureWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			path := filepath.Join(t.TempDir(), "media")
			if err := os.WriteFile(path, oggBytes, 0600); err != nil {
				t.Fatal(err)
			}
			calls := stubAutomaticDownloads(t, path, false)
			handleMessage(newTestClient(&mockLIDStore{}), newTestMessageStore(t), messageWith(tc.msg, "media-1"), testLogger())
			select {
			case p := <-webhookCh:
				if !tc.wantWebhook {
					t.Fatalf("unexpected webhook: %+v", p)
				}
				tc.check(t, p, calls)
			default:
				if tc.wantWebhook {
					t.Fatal("expected a webhook")
				}
			}
		})
	}
}

func TestHandleMessage_LocationWebhook(t *testing.T) {
	pin := &waProto.Message{LocationMessage: &waProto.LocationMessage{
		DegreesLatitude: proto.Float64(41.9), DegreesLongitude: proto.Float64(12.5),
		Name: proto.String("Colonia felina"), Address: proto.String("Via Appia 1"),
	}}
	t.Setenv("WEBHOOK_ENABLED", "true")

	t.Run("default does not forward", func(t *testing.T) {
		t.Setenv("WEBHOOK_MEDIA_TYPES", "")
		srv, ch := captureRawWebhook(t)
		t.Setenv("WEBHOOK_URL", srv.URL)
		handleMessage(newTestClient(&mockLIDStore{}), newTestMessageStore(t), messageWith(pin, "loc-0"), testLogger())
		select {
		case p := <-ch:
			t.Fatalf("unexpected webhook: %v", p)
		default:
		}
	})

	t.Run("pin", func(t *testing.T) {
		t.Setenv("WEBHOOK_MEDIA_TYPES", "image,location")
		srv, ch := captureRawWebhook(t)
		t.Setenv("WEBHOOK_URL", srv.URL)
		handleMessage(newTestClient(&mockLIDStore{}), newTestMessageStore(t), messageWith(pin, "loc-1"), testLogger())
		select {
		case p := <-ch:
			if p["mediaType"] != "location" || p["latitude"] != 41.9 || p["longitude"] != 12.5 ||
				p["locationName"] != "Colonia felina" || p["locationAddress"] != "Via Appia 1" || p["messageId"] != "loc-1" {
				t.Errorf("location payload = %v", p)
			}
			if _, ok := p["mediaBase64"]; ok {
				t.Error("location must carry no bytes")
			}
		default:
			t.Fatal("expected a location webhook")
		}
	})

	t.Run("live location sends only its first fix", func(t *testing.T) {
		t.Setenv("WEBHOOK_MEDIA_TYPES", "location")
		live := &waProto.Message{LiveLocationMessage: &waProto.LiveLocationMessage{
			DegreesLatitude: proto.Float64(45.4), DegreesLongitude: proto.Float64(9.2),
		}}
		var got []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			got = append(got, p)
		}))
		t.Cleanup(srv.Close)
		t.Setenv("WEBHOOK_URL", srv.URL)
		chat := types.NewJID("393330000099", types.DefaultUserServer)
		for i, id := range []string{"live-1", "live-2"} {
			msg := messageWith(live, id)
			msg.Info.Chat, msg.Info.Sender = chat, chat
			msg.Info.Timestamp = time.Now().Add(time.Duration(i) * time.Minute)
			handleMessage(newTestClient(&mockLIDStore{}), newTestMessageStore(t), msg, testLogger())
		}
		if len(got) != 1 || got[0]["messageId"] != "live-1" || got[0]["locationName"] != "" {
			t.Fatalf("live location webhooks = %v, want only the first fix", got)
		}
	})
}

func TestShouldForwardReceipt(t *testing.T) {
	const dm, group = "393330000001@s.whatsapp.net", "120363000000000001@g.us"
	t.Setenv("WEBHOOK_ENABLED", "true")
	for _, tc := range []struct {
		setting, chat, kind string
		want                bool
	}{
		{"", dm, "read", false}, // default off
		{"true", dm, "read", true},
		{"true", dm, "delivered", true},
		{"true", dm, "played", false},
		{"true", group, "read", false},
		{"true", "123@lid", "read", false},
	} {
		t.Setenv("WEBHOOK_RECEIPTS", tc.setting)
		if got := shouldForwardReceipt(tc.chat, tc.kind); got != tc.want {
			t.Errorf("WEBHOOK_RECEIPTS=%q %s %s = %v, want %v", tc.setting, tc.chat, tc.kind, got, tc.want)
		}
	}
}

func TestSendReceiptWebhookPayload(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "true")
	srv, ch := captureRawWebhook(t)
	t.Setenv("WEBHOOK_URL", srv.URL)
	at := time.Date(2026, 11, 7, 21, 0, 0, 0, time.FixedZone("CET", 3600))
	SendReceiptWebhook("393330000001", "393330000001@s.whatsapp.net", "read", []string{"m1", "m2"}, at)
	p := <-ch
	ids, _ := p["messageIds"].([]any)
	if p["eventType"] != "receipt" || p["chatJID"] != "393330000001@s.whatsapp.net" || p["receiptType"] != "read" ||
		len(ids) != 2 || ids[0] != "m1" || p["timestamp"] != "2026-11-07T20:00:00Z" {
		t.Fatalf("receipt payload = %v", p)
	}
}

func TestSendHandler_ResponseCarriesID(t *testing.T) {
	const token = "supersecrettoken1234567890abcdef"
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, token, nil)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send",
		strings.NewReader(`{"recipient":"393330000001","message":"ciao"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", resp.Body.String(), err)
	}
	// the test client is not connected: the send fails and id is present but empty
	if id, ok := body["id"]; !ok || id != "" || body["success"] != false {
		t.Fatalf("response = %v, want success false and an empty id", body)
	}
}
