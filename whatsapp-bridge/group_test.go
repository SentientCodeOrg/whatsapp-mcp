package main

import (
	"bytes"
	"image"
	"image/jpeg"
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

func TestSquareJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 700))
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	out, err := squareJPEG(src, groupPhotoSide)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 640 || b.Dy() != 640 {
		t.Fatalf("want 640x640, got %v", b)
	}
	if r, _, _, _ := img.At(320, 320).RGBA(); r>>8 < 190 || r>>8 > 210 {
		t.Fatalf("colour drifted: %d", r>>8)
	}
	small, _ := squareJPEG(image.NewRGBA(image.Rect(0, 0, 300, 400)), groupPhotoSide)
	if img, _ := jpeg.Decode(bytes.NewReader(small)); img.Bounds().Dx() != 300 {
		t.Fatalf("small images must not be upscaled, got %v", img.Bounds())
	}
}
