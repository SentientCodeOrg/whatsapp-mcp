package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// WhatsApp's own limits for a poll: 2 to 12 options.
const (
	minPollOptions = 2
	maxPollOptions = 12
)

// pollCreation returns a message's poll, whichever of the three wire versions
// carries it, or nil.
func pollCreation(msg *waProto.Message) *waProto.PollCreationMessage {
	for _, p := range []*waProto.PollCreationMessage{msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func pollOptionNames(p *waProto.PollCreationMessage) []string {
	names := make([]string, 0, len(p.GetOptions()))
	for _, o := range p.GetOptions() {
		names = append(names, o.GetOptionName())
	}
	return names
}

// pollContent is how a poll reads in the messages table.
func pollContent(p *waProto.PollCreationMessage) string {
	var b strings.Builder
	b.WriteString("📊 " + p.GetName())
	for i, name := range pollOptionNames(p) {
		fmt.Fprintf(&b, "\n%d. %s", i+1, name)
	}
	if p.GetSelectableOptionsCount() != 1 {
		b.WriteString("\n(more than one answer allowed)")
	}
	return b.String()
}

// recordPoll keeps a poll's options so its votes, which carry only option
// hashes, can be read back as names. Messages that aren't polls are ignored.
func recordPoll(store *MessageStore, id, chatJID string, msg *waProto.Message) {
	p := pollCreation(msg)
	if p == nil || id == "" {
		return
	}
	opts, _ := json.Marshal(pollOptionNames(p))
	if _, err := store.db.Exec(
		`INSERT INTO polls (id, chat_jid, question, options) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id, chat_jid) DO NOTHING`, id, chatJID, p.GetName(), string(opts)); err != nil {
		fmt.Printf("Warning: failed to record poll %s: %v\n", id, err)
	}
}

// matchPollVote maps the selected option hashes back to option names.
func matchPollVote(options []string, selected [][]byte) []string {
	hashes := whatsmeow.HashPollOptions(options)
	chosen := []string{}
	for _, sel := range selected {
		for i, h := range hashes {
			if bytes.Equal(sel, h) {
				chosen = append(chosen, options[i])
				break
			}
		}
	}
	return chosen
}

// handlePollVote decrypts a vote and stores the voter's current choice (an
// empty list when they withdrew it). Votes on polls we never saw are skipped.
func handlePollVote(client *whatsmeow.Client, store *MessageStore, evt *events.Message, chatJID, voter string, logger waLog.Logger) {
	update := evt.Message.GetPollUpdateMessage()
	if update == nil {
		return
	}
	pollID := update.GetPollCreationMessageKey().GetID()
	var raw string
	if err := store.db.QueryRow(`SELECT options FROM polls WHERE id = ? AND chat_jid = ?`, pollID, chatJID).Scan(&raw); err != nil {
		logger.Infof("Vote on unknown poll %s in %s: skipped", pollID, chatJID)
		return
	}
	vote, err := client.DecryptPollVote(context.Background(), evt)
	if err != nil {
		logger.Warnf("Failed to decrypt vote on poll %s: %v", pollID, err)
		return
	}
	var options []string
	_ = json.Unmarshal([]byte(raw), &options)
	chosen, _ := json.Marshal(matchPollVote(options, vote.GetSelectedOptions()))
	if _, err := store.db.Exec(
		`INSERT INTO poll_votes (poll_id, chat_jid, voter, options, timestamp) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(poll_id, chat_jid, voter) DO UPDATE SET options = excluded.options, timestamp = excluded.timestamp
		 WHERE excluded.timestamp >= poll_votes.timestamp`,
		pollID, chatJID, voter, string(chosen), evt.Info.Timestamp); err != nil {
		logger.Warnf("Failed to store vote on poll %s: %v", pollID, err)
	}
}

// registerPollEndpoint adds POST /api/poll {recipient, question, options, multiple}.
func registerPollEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, store *MessageStore) {
	mux.HandleFunc("/api/poll", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Recipient string   `json:"recipient"`
			Question  string   `json:"question"`
			Options   []string `json:"options"`
			Multiple  bool     `json:"multiple"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		req.Question = strings.TrimSpace(req.Question)
		var options []string
		seen := map[string]bool{}
		for _, o := range req.Options {
			if o = strings.TrimSpace(o); o != "" && !seen[o] {
				seen[o] = true
				options = append(options, o)
			}
		}
		if req.Recipient == "" || req.Question == "" || len(options) < minPollOptions || len(options) > maxPollOptions {
			fail(http.StatusBadRequest, fmt.Sprintf("recipient, question and %d-%d distinct options are required", minPollOptions, maxPollOptions))
			return
		}
		selectable := 1
		if req.Multiple {
			selectable = 0 // any number
		}
		msg := client.BuildPollCreation(req.Question, options, selectable)
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		id, chatJID, err := sendBuilt(ctx, client, store, req.Recipient, msg, pollContent(msg.GetPollCreationMessage()))
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		recordPoll(store, id, chatJID, msg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": id, "chat_jid": chatJID})
	}))
}
