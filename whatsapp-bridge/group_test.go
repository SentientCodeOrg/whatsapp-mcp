package main

import "testing"

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
