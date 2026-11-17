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
		fail := jsonFail(w)
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

		var mine []types.GroupParticipant
		for _, p := range info.Participants {
			if !p.IsSuperAdmin {
				mine = append(mine, p)
			}
		}
		resp := participantResult(ctx, client, info.JID, mine)
		resp["jid"], resp["name"] = info.JID.String(), req.Name
		writeJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/group/info", auth(func(w http.ResponseWriter, r *http.Request) {
		fail := jsonFail(w)
		group, err := parseGroupJID(r.URL.Query().Get("jid"))
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		info, err := client.GetGroupInfo(ctx, group)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("group info failed: %v", err))
			return
		}
		members := make([]map[string]any, 0, len(info.Participants))
		for _, p := range info.Participants {
			members = append(members, map[string]any{
				"phone": phoneOf(p), "jid": p.JID.String(), "admin": p.IsAdmin || p.IsSuperAdmin, "owner": p.IsSuperAdmin,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "jid": info.JID.String(), "name": info.Name, "description": info.Topic,
			"created": info.GroupCreated, "only_admins_send": info.IsAnnounce, "only_admins_edit_info": info.IsLocked,
			"disappearing_seconds": info.DisappearingTimer, "participants": members,
		})
	}))

	mux.HandleFunc("/api/group/participants", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group        string   `json:"group"`
			Action       string   `json:"action"`
			Participants []string `json:"participants"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		group, err := parseGroupJID(req.Group)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		action := whatsmeow.ParticipantChange(req.Action)
		switch action {
		case whatsmeow.ParticipantChangeAdd, whatsmeow.ParticipantChangeRemove, whatsmeow.ParticipantChangePromote, whatsmeow.ParticipantChangeDemote:
		default:
			fail(http.StatusBadRequest, "action must be add, remove, promote or demote")
			return
		}
		people, err := parseGroupParticipants(req.Participants)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := client.UpdateGroupParticipants(ctx, group, people, action)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("%s failed: %v", req.Action, err))
			return
		}
		out := participantResult(ctx, client, group, res)
		if action != whatsmeow.ParticipantChangeAdd {
			delete(out, "invite_link") // only useful to people we could not add
		}
		writeJSON(w, http.StatusOK, out)
	}))

	mux.HandleFunc("/api/group/settings", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group          string  `json:"group"`
			Name           *string `json:"name"`
			Description    *string `json:"description"`
			OnlyAdminsSend *bool   `json:"only_admins_send"`
			OnlyAdminsEdit *bool   `json:"only_admins_edit_info"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		group, err := parseGroupJID(req.Group)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if req.Name != nil && (strings.TrimSpace(*req.Name) == "" || utf8.RuneCountInString(*req.Name) > maxGroupNameRunes) {
			fail(http.StatusBadRequest, fmt.Sprintf("name must be 1-%d characters", maxGroupNameRunes))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var changed []string
		step := func(what string, err error) bool {
			if err != nil {
				fail(http.StatusBadGateway, fmt.Sprintf("changing %s failed: %v (already changed: %v)", what, err, changed))
				return false
			}
			changed = append(changed, what)
			return true
		}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if !step("name", client.SetGroupName(ctx, group, name)) {
				return
			}
			_ = messageStore.StoreChatName(group.String(), name)
		}
		if req.Description != nil {
			info, err := client.GetGroupInfo(ctx, group)
			if err == nil {
				err = client.SetGroupTopic(ctx, group, info.TopicID, "", *req.Description)
			}
			if !step("description", err) {
				return
			}
		}
		if req.OnlyAdminsSend != nil && !step("only_admins_send", client.SetGroupAnnounce(ctx, group, *req.OnlyAdminsSend)) {
			return
		}
		if req.OnlyAdminsEdit != nil && !step("only_admins_edit_info", client.SetGroupLocked(ctx, group, *req.OnlyAdminsEdit)) {
			return
		}
		if len(changed) == 0 {
			fail(http.StatusBadRequest, "nothing to change: pass name, description, only_admins_send or only_admins_edit_info")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed})
	}))

	mux.HandleFunc("/api/group/invite", auth(func(w http.ResponseWriter, r *http.Request) {
		fail := jsonFail(w)
		group, err := parseGroupJID(r.URL.Query().Get("jid"))
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		link, err := client.GetGroupInviteLink(r.Context(), group, r.URL.Query().Get("reset") == "1")
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("invite link failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "invite_link": link})
	}))

	mux.HandleFunc("/api/group/leave", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group string `json:"group"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		group, err := parseGroupJID(req.Group)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if err := client.LeaveGroup(r.Context(), group); err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("leave failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/group/join", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Link string `json:"link"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		code := inviteCode(req.Link)
		if code == "" {
			fail(http.StatusBadRequest, "link must be a chat.whatsapp.com invite link or its code")
			return
		}
		jid, err := client.JoinGroupWithLink(r.Context(), code)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("join failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jid": jid.String()})
	}))
}

// participantResult splits a participant change into who it worked for and
// who WhatsApp refused (error code, usually their privacy settings), adding
// the group's invite link when someone was refused.
func participantResult(ctx context.Context, client *whatsmeow.Client, group types.JID, res []types.GroupParticipant) map[string]any {
	done, failed := []string{}, []map[string]any{}
	for _, p := range res {
		if p.Error != 0 {
			failed = append(failed, map[string]any{"participant": phoneOf(p), "error": p.Error})
		} else {
			done = append(done, phoneOf(p))
		}
	}
	out := map[string]any{"ok": true, "done": done, "failed": failed}
	if len(failed) > 0 {
		if link, err := client.GetGroupInviteLink(ctx, group, false); err == nil {
			out["invite_link"] = link
		}
	}
	return out
}

func phoneOf(p types.GroupParticipant) string {
	if !p.PhoneNumber.IsEmpty() {
		return p.PhoneNumber.User
	}
	return p.JID.User
}

func parseGroupJID(s string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(s))
	if err != nil || jid.User == "" || jid.Server != types.GroupServer {
		return types.JID{}, fmt.Errorf("group must be a group JID (…@g.us)")
	}
	return jid, nil
}

// inviteCode takes "https://chat.whatsapp.com/CODE" (or the bare code).
func inviteCode(link string) string {
	link = strings.TrimSpace(link)
	if i := strings.LastIndex(link, "/"); i >= 0 {
		link = link[i+1:]
	}
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		link = link[:i]
	}
	return link
}
