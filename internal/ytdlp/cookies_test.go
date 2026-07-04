package ytdlp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseDomain(t *testing.T) {
	cases := map[string]string{
		"www.instagram.com": "instagram.com",
		"instagram.com":     "instagram.com",
		"i.instagram.com":   "instagram.com",
		".instagram.com":    "instagram.com", // leading dot (cookie domain form)
		"www.bbc.co.uk":     "bbc.co.uk",      // multi-part TLD
		"example.com":       "example.com",
		"localhost":         "localhost",
	}
	for in, want := range cases {
		if got := baseDomain(in); got != want {
			t.Errorf("baseDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteAndMatchCookies(t *testing.T) {
	dir := t.TempDir()
	SetCookiesDir(dir)
	t.Cleanup(func() { SetCookiesDir("") })

	err := WriteCookies("https://www.instagram.com/reel/ABC/", []Cookie{
		{Name: "sessionid", Value: "s%3Av", Domain: ".instagram.com", Path: "/", Secure: true, HTTPOnly: true, Expiry: 1799999999},
		{Name: "csrftoken", Value: "tok", Domain: ".instagram.com", Path: "/", Secure: true, Expiry: 0}, // session cookie
		{Name: "ds_user_id", Value: "9", Domain: "www.instagram.com", Path: "/", Secure: true, HostOnly: true, Expiry: 1799999999},
	})
	if err != nil {
		t.Fatalf("WriteCookies: %v", err)
	}

	// One jar per registrable domain, reused across subdomains.
	jar := filepath.Join(dir, "instagram.com.txt")
	body, err := os.ReadFile(jar)
	if err != nil {
		t.Fatalf("read jar: %v", err)
	}
	txt := string(body)
	if !strings.HasPrefix(txt, "# Netscape HTTP Cookie File") {
		t.Errorf("missing Netscape header:\n%s", txt)
	}
	// httpOnly cookies MUST carry the #HttpOnly_ prefix or yt-dlp drops the auth cookie.
	if !strings.Contains(txt, "#HttpOnly_.instagram.com\tTRUE\t/\tTRUE\t1799999999\tsessionid\ts%3Av") {
		t.Errorf("sessionid line wrong:\n%s", txt)
	}
	// hostOnly => include-subdomains FALSE, exact host, no #HttpOnly_ prefix.
	if !strings.Contains(txt, "\nwww.instagram.com\tFALSE\t/\tTRUE\t1799999999\tds_user_id\t9") {
		t.Errorf("ds_user_id line wrong:\n%s", txt)
	}

	// cookieArgsFor resolves the jar for any instagram.com URL / subdomain.
	for _, u := range []string{
		"https://www.instagram.com/reel/ABC/",
		"https://instagram.com/stories/x/1/",
		"https://i.instagram.com/api/v1/",
	} {
		args := cookieArgsFor(u)
		if len(args) != 2 || args[0] != "--cookies" || args[1] != jar {
			t.Errorf("cookieArgsFor(%q) = %v, want [--cookies %s]", u, args, jar)
		}
	}

	// A domain with no jar gets nothing (yt-dlp stays anonymous, as before).
	if args := cookieArgsFor("https://www.youtube.com/watch?v=x"); args != nil {
		t.Errorf("cookieArgsFor(youtube) = %v, want nil", args)
	}

	// Empty cookie list removes the jar.
	if err := WriteCookies("https://www.instagram.com/", nil); err != nil {
		t.Fatalf("WriteCookies(nil): %v", err)
	}
	if _, err := os.Stat(jar); !os.IsNotExist(err) {
		t.Errorf("jar should be removed, stat err = %v", err)
	}
}
