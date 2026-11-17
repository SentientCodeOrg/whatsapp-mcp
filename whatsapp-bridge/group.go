package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// maxGroupNameRunes is WhatsApp's limit; a longer name gets 406 from the server.
const maxGroupNameRunes = 25

// parseGroupParticipants turns phone numbers ("+39 333 1234567") or user JIDs
// into the JIDs CreateGroup wants. Duplicates are dropped.
func parseGroupParticipants(in []string) ([]types.JID, error) {
	seen := map[types.JID]bool{}
	var out []types.JID
	for _, p := range in {
		p = strings.TrimSpace(p)
		var jid types.JID
		if strings.Contains(p, "@") {
			parsed, err := types.ParseJID(p)
			if err != nil || parsed.User == "" || (parsed.Server != types.DefaultUserServer && parsed.Server != types.HiddenUserServer) {
				return nil, fmt.Errorf("participant %q is not a user JID", p)
			}
			jid = parsed.ToNonAD()
		} else {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, p)
			if len(digits) < 6 {
				return nil, fmt.Errorf("participant %q is not a phone number", p)
			}
			jid = types.NewJID(digits, types.DefaultUserServer)
		}
		if !seen[jid] {
			seen[jid] = true
			out = append(out, jid)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one participant is required")
	}
	return out, nil
}

// registerGroupEndpoint adds POST /api/group/create {name, participants}: a new
// group with this account as admin. Participants WhatsApp refuses to add
// (privacy settings) come back in "failed", with an invite link to send them.
func registerGroupEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, messageStore *MessageStore) {
	mux.HandleFunc("/api/group/create", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name         string   `json:"name"`
			Participants []string `json:"participants"`
		}
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || utf8.RuneCountInString(req.Name) > maxGroupNameRunes {
			fail(http.StatusBadRequest, fmt.Sprintf("name must be 1-%d characters", maxGroupNameRunes))
			return
		}
		participants, err := parseGroupParticipants(req.Participants)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		info, err := client.CreateGroup(ctx, whatsmeow.ReqCreateGroup{Name: req.Name, Participants: participants})
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("create group failed: %v", err))
			return
		}
		_ = messageStore.StoreChat(info.JID.String(), req.Name, time.Now())

		var added []string
		var failed []map[string]any
		for _, p := range info.Participants {
			who := p.PhoneNumber
			if who.IsEmpty() {
				who = p.JID
			}
			if p.Error != 0 {
				failed = append(failed, map[string]any{"participant": who.User, "error": p.Error})
			} else if !p.IsSuperAdmin {
				added = append(added, who.User)
			}
		}
		resp := map[string]any{"ok": true, "jid": info.JID.String(), "name": req.Name, "added": added, "failed": failed}
		if len(failed) > 0 {
			if link, err := client.GetGroupInviteLink(ctx, info.JID, false); err == nil {
				resp["invite_link"] = link
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}
