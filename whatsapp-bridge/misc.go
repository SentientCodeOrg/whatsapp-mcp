package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonFail returns a writer of {"ok": false, "error": msg} for one response.
func jsonFail(w http.ResponseWriter) func(int, string) {
	return func(code int, msg string) { writeJSON(w, code, map[string]any{"ok": false, "error": msg}) }
}

// decodePost checks the method and decodes the JSON body, answering the
// request itself when either is wrong.
func decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		jsonFail(w)(http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonFail(w)(http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// parseChat takes a phone number or a user/group JID, as /api/send does.
func parseChat(recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)
	if strings.Contains(recipient, "@") {
		return types.ParseJID(recipient)
	}
	people, err := parseGroupParticipants([]string{recipient})
	if err != nil {
		return types.JID{}, err
	}
	return people[0], nil
}

// sendBuilt sends a ready message the way /api/send does (LID addressing,
// the chat's disappearing timer) and records it as ours in the store. It
// returns the message id and the chat JID it was stored under.
func sendBuilt(ctx context.Context, client *whatsmeow.Client, store *MessageStore, recipient string, msg *waProto.Message, content string) (string, string, error) {
	if !client.IsConnected() || client.Store.ID == nil {
		return "", "", fmt.Errorf("not connected to WhatsApp")
	}
	chat, err := parseChat(recipient)
	if err != nil {
		return "", "", err
	}
	to, err := resolveRecipientJID(ctx, client, chat.String())
	if err != nil {
		return "", "", err
	}
	stored := resolveUserJID(client, chat, types.EmptyJID).String()
	if settings, err := store.GetChatEphemeralSettings(stored); err == nil {
		applyChatEphemeralSettings(msg, settings)
	}
	resp, err := client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", "", fmt.Errorf("send failed: %v", err)
	}
	ts := resp.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	_ = store.StoreChat(stored, "", ts)
	if err := store.StoreMessage(resp.ID, stored, client.Store.ID.User, content, ts, true,
		"", "", "", nil, nil, nil, 0, "", 0); err != nil {
		fmt.Printf("Warning: sent %s but failed to store it: %v\n", resp.ID, err)
	}
	return resp.ID, stored, nil
}

func contactVCard(name, phone string) string {
	return fmt.Sprintf("BEGIN:VCARD\nVERSION:3.0\nFN:%s\nTEL;type=CELL;waid=%s:+%s\nEND:VCARD", name, phone, phone)
}

func registerMiscEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, store *MessageStore) {
	sent := func(w http.ResponseWriter, r *http.Request, recipient string, msg *waProto.Message, content string) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		id, chat, err := sendBuilt(ctx, client, store, recipient, msg, content)
		if err != nil {
			jsonFail(w)(http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": id, "chat_jid": chat})
	}

	mux.HandleFunc("/api/send-location", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Recipient string  `json:"recipient"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Name      string  `json:"name"`
			Address   string  `json:"address"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		if req.Recipient == "" || req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 ||
			(req.Latitude == 0 && req.Longitude == 0) {
			jsonFail(w)(http.StatusBadRequest, "recipient and a valid latitude/longitude are required")
			return
		}
		loc := &waProto.LocationMessage{DegreesLatitude: proto.Float64(req.Latitude), DegreesLongitude: proto.Float64(req.Longitude)}
		if req.Name != "" {
			loc.Name = proto.String(req.Name)
		}
		if req.Address != "" {
			loc.Address = proto.String(req.Address)
		}
		content := strings.TrimSpace(fmt.Sprintf("📍 %s %s (%.6f, %.6f)", req.Name, req.Address, req.Latitude, req.Longitude))
		sent(w, r, req.Recipient, &waProto.Message{LocationMessage: loc}, content)
	}))

	mux.HandleFunc("/api/send-contact", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Recipient string `json:"recipient"`
			Name      string `json:"name"`
			Phone     string `json:"phone"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		people, err := parseGroupParticipants([]string{req.Phone})
		if err != nil || req.Recipient == "" || req.Name == "" || people[0].Server != types.DefaultUserServer {
			jsonFail(w)(http.StatusBadRequest, "recipient, name and the contact's phone number are required")
			return
		}
		phone := people[0].User
		msg := &waProto.Message{ContactMessage: &waProto.ContactMessage{
			DisplayName: proto.String(req.Name), Vcard: proto.String(contactVCard(req.Name, phone)),
		}}
		sent(w, r, req.Recipient, msg, fmt.Sprintf("📇 %s +%s", req.Name, phone))
	}))

	mux.HandleFunc("/api/check-numbers", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Phones []string `json:"phones"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		people, err := parseGroupParticipants(req.Phones)
		if err != nil || len(people) > 50 {
			jsonFail(w)(http.StatusBadRequest, "1-50 phone numbers with country code are required")
			return
		}
		query := make([]string, 0, len(people))
		for _, p := range people {
			if p.Server != types.DefaultUserServer {
				jsonFail(w)(http.StatusBadRequest, "phones must be phone numbers, not JIDs")
				return
			}
			query = append(query, "+"+p.User)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := client.IsOnWhatsApp(ctx, query)
		if err != nil {
			jsonFail(w)(http.StatusBadGateway, fmt.Sprintf("lookup failed: %v", err))
			return
		}
		var registered []types.JID
		for _, x := range res {
			if x.IsIn {
				registered = append(registered, x.JID)
			}
		}
		infos := map[types.JID]types.UserInfo{}
		if len(registered) > 0 {
			if got, err := client.GetUserInfo(ctx, registered); err == nil {
				infos = got
			}
		}
		// WhatsApp leaves unregistered numbers out of its answer entirely.
		byQuery := map[string]types.IsOnWhatsAppResponse{}
		for _, x := range res {
			byQuery[strings.TrimPrefix(x.Query, "+")] = x
		}
		out := make([]map[string]any, 0, len(query))
		for _, q := range query {
			x := byQuery[strings.TrimPrefix(q, "+")]
			row := map[string]any{"phone": strings.TrimPrefix(q, "+"), "on_whatsapp": x.IsIn}
			if x.IsIn {
				row["jid"] = x.JID.String()
				if info, ok := infos[x.JID]; ok && strings.TrimSpace(info.Status) != "" {
					row["about"] = info.Status
				}
				if x.VerifiedName != nil && x.VerifiedName.Details != nil {
					row["business_name"] = x.VerifiedName.Details.GetVerifiedName()
				}
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": out})
	}))

	mux.HandleFunc("/api/disappearing", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Chat  string `json:"chat"`
			Timer string `json:"timer"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Timer)
		chat, err := parseChat(req.Chat)
		if !ok || err != nil {
			jsonFail(w)(http.StatusBadRequest, "chat (phone or JID) and timer (off, 24h, 7d or 90d) are required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		target := chat
		if chat.Server != types.GroupServer {
			if target, err = resolveRecipientJID(ctx, client, chat.String()); err != nil {
				jsonFail(w)(http.StatusBadRequest, err.Error())
				return
			}
		}
		now := time.Now()
		if err := client.SetDisappearingTimer(ctx, target, timer, now); err != nil {
			jsonFail(w)(http.StatusBadGateway, fmt.Sprintf("setting the timer failed: %v", err))
			return
		}
		// Our own change never comes back as an event in a one-to-one chat;
		// record it so the next message we send carries the timer.
		stored := resolveUserJID(client, chat, types.EmptyJID).String()
		if err := store.UpdateChatEphemeralSettings(stored, uint32(timer.Seconds()), now.Unix()); err != nil {
			fmt.Printf("Warning: timer set but not recorded for %s: %v\n", stored, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat_jid": stored, "timer_seconds": int(timer.Seconds())})
	}))

	mux.HandleFunc("/api/block", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Contact string `json:"contact"`
			Action  string `json:"action"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		action := events.BlocklistChangeAction(req.Action)
		people, err := parseGroupParticipants([]string{req.Contact})
		if err != nil || (action != events.BlocklistChangeActionBlock && action != events.BlocklistChangeActionUnblock) {
			jsonFail(w)(http.StatusBadRequest, "contact (phone or user JID) and action (block or unblock) are required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if _, err := client.UpdateBlocklist(ctx, people[0], action); err != nil {
			jsonFail(w)(http.StatusBadGateway, fmt.Sprintf("%s failed: %v", req.Action, err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
}
