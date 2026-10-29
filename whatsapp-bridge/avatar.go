package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// maxAvatarBytes caps a profile picture download; WhatsApp's previews are a few KB.
const maxAvatarBytes = 2 << 20

// registerAvatarEndpoint adds GET /api/avatar?jid=..., the profile picture
// (preview size) of a user or group as WhatsApp shows it to this account:
// the image bytes, or 404 when there is none or its owner hides it from us.
// The bridge downloads it itself, so callers never contact WhatsApp's CDN.
func registerAvatarEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client) {
	mux.HandleFunc("/api/avatar", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jid, err := types.ParseJID(r.URL.Query().Get("jid"))
		if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer) {
			http.Error(w, "jid must be a user or group JID", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		info, err := client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true})
		if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) ||
			(err == nil && (info == nil || info.URL == "")) {
			http.Error(w, "no profile picture", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("profile picture lookup failed: %v", err), http.StatusBadGateway)
			return
		}
		if !strings.HasPrefix(info.URL, "https://") {
			http.Error(w, "unexpected picture URL", http.StatusBadGateway)
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("picture download failed: %v", err), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxAvatarBytes+1))
		if err != nil || resp.StatusCode != http.StatusOK || len(body) > maxAvatarBytes {
			http.Error(w, "picture download failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Picture-Id", info.ID)
		_, _ = w.Write(body)
	}))
}
