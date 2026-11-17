package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestParseGroupParticipants(t *testing.T) {
	got, err := parseGroupParticipants([]string{"+39 333 123 4567", "393331234567", "123@lid", "39444@s.whatsapp.net"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"393331234567@s.whatsapp.net", "123@lid", "39444@s.whatsapp.net"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	for _, bad := range [][]string{nil, {"12"}, {"123@g.us"}} {
		if _, err := parseGroupParticipants(bad); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestInviteCode(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.whatsapp.com/AbC123?mode=r": "AbC123",
		" AbC123 ": "AbC123",
		"":         "",
	} {
		if got := inviteCode(in); got != want {
			t.Errorf("inviteCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchPollVote(t *testing.T) {
	opts := []string{"Sì", "No", "Forse"}
	hashes := whatsmeow.HashPollOptions([]string{"Forse", "Sì", "altro"})
	got := matchPollVote(opts, hashes)
	if len(got) != 2 || got[0] != "Forse" || got[1] != "Sì" {
		t.Errorf("matchPollVote = %v, want [Forse Sì]", got)
	}
	if got := matchPollVote(opts, nil); got == nil || len(got) != 0 {
		t.Errorf("withdrawn vote = %#v, want empty non-nil list", got)
	}
}
