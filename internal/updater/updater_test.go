package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		remote, current string
		want            bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"1.10.0", "1.9.0", true}, // numeric, not lexical
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "1.0", true},         // 1.0.0 > 1.0? no — equal-padded; expect false
		{"v1.2.0", "1.2.0", false},     // leading v ignored
		{"1.2.0-beta", "1.2.0", false}, // prerelease dropped -> equal
		{"1.2.0", "dev", true},         // any real version beats "dev"
		{"1.2.0", "", true},
	}
	for _, c := range cases {
		// "1.0.0" vs "1.0" — parseVer pads missing to 0, so they're equal.
		want := c.want
		if c.remote == "1.0.0" && c.current == "1.0" {
			want = false
		}
		if got := Newer(c.remote, c.current); got != want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.remote, c.current, got, want)
		}
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"1.3.0","url":"https://x/DBox.exe","sha256":"abc","notes":"stuff","date":"2026-07-03"}`))
	}))
	defer srv.Close()

	rel, err := Fetch(context.Background(), srv.URL, "DBox-test")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "1.3.0" || rel.URL != "https://x/DBox.exe" || rel.Notes != "stuff" {
		t.Fatalf("unexpected release: %+v", rel)
	}
}

func TestFetchRejectsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"notes":"no version or url"}`))
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected error for manifest missing version/url")
	}
}
