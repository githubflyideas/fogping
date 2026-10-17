package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)

func TestParseSponsorManifestValidates(t *testing.T) {
	raw := `{"version":2,"slides":[
	 {"image":"a.png","link":"https://x.example/?utm_source=t","alt":"A"},
	 {"image":"http://insecure.example/b.png","link":"https://x.example","alt":"B"},
	 {"image":"https://cdn.example/c.png","link":"javascript:alert(1)","alt":"C"},
	 {"image":"data:image/png;base64,AAAA","alt":"D"},
	 {"image":"d.png","link":"http://plain.example","alt":"E"},
	 {"image":"e.png","alt":"F"},
	 {"image":"f.png","alt":"G"},
	 {"image":"g.png","alt":"H"}]}`
	got, err := parseSponsorManifest([]byte(raw), "https://raw.example/ads/p/sponsors.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != sponsorMaxSlides {
		t.Fatalf("got %d slides, want %d", len(got), sponsorMaxSlides)
	}
	if got[0].Image != "https://raw.example/ads/p/a.png" || got[0].Link != "https://x.example/?utm_source=t" {
		t.Errorf("relative image / utm link not kept: %+v", got[0])
	}
	if got[1].Alt != "C" || got[1].Link != "" {
		t.Errorf("javascript: link must be dropped, insecure image skipped: %+v", got[1])
	}
	if got[2].Link != "http://plain.example" {
		t.Errorf("http link should be allowed: %+v", got[2])
	}
	for _, bad := range []string{`not json`, `{"slides":[]}`, `{"slides":[{"image":"ftp://x/y.png"}]}`} {
		if _, err := parseSponsorManifest([]byte(bad), "https://raw.example/m.json"); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSniffSponsorImage(t *testing.T) {
	if ct, err := sniffSponsorImage(testPNG); err != nil || ct != "image/png" {
		t.Errorf("png: %q %v", ct, err)
	}
	for _, b := range [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), []byte("short"), bytes.Repeat([]byte("x"), 40)} {
		if _, err := sniffSponsorImage(b); err == nil {
			t.Errorf("%q should be refused", b[:5])
		}
	}
}

func sponsorTestServer(t *testing.T, files map[string][]byte) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testCache(t *testing.T, srv *httptest.Server, dir string) *sponsorCache {
	c := newSponsorCache("test", dir)
	c.src = srv.URL + "/p/sponsors.json"
	c.client = srv.Client()
	return c
}

func TestSponsorRefreshCachesAndServes(t *testing.T) {
	files := map[string][]byte{
		"/p/sponsors.json": []byte(`{"slides":[
		 {"image":"one.png","link":"https://ad.example/?utm_source=x","alt":"One"},
		 {"image":"evil.svg","alt":"SVG"},
		 {"image":"missing.png","alt":"Gone"},
		 {"image":"two.png","alt":"Two","title":"T"}]}`),
		"/p/one.png":  testPNG,
		"/p/two.png":  append(append([]byte{}, testPNG...), 1),
		"/p/evil.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	}
	srv := sponsorTestServer(t, files)
	dir := t.TempDir()
	c := testCache(t, srv, dir)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A fresh cache over the same directory — a restart — must serve the same
	// slides without any network.
	sponsor = newSponsorCache("test", dir)
	defer func() { sponsor = nil }()
	mux := http.NewServeMux()
	registerSponsor(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sponsor/sponsors.json", nil))
	var m sponsorManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Slides) != 2 || m.Slides[0].Alt != "One" || m.Slides[1].Alt != "Two" {
		t.Fatalf("slides = %+v", m.Slides)
	}
	if !strings.HasPrefix(m.Slides[0].Image, "/sponsor/img/0?v=") {
		t.Errorf("image not rewritten to local: %s", m.Slides[0].Image)
	}
	if m.Slides[0].Link != "https://ad.example/?utm_source=x" {
		t.Errorf("link changed: %s", m.Slides[0].Link)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sponsor/img/1", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), files["/p/two.png"]) {
		t.Errorf("img/1: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/sponsor/img/2", "/sponsor/img/-1", "/sponsor/img/x"} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
}

func TestSponsorFailureKeepsLastGoodCopy(t *testing.T) {
	files := map[string][]byte{
		"/p/sponsors.json": []byte(`{"slides":[{"image":"one.png","alt":"One"}]}`),
		"/p/one.png":       testPNG,
	}
	srv := sponsorTestServer(t, files)
	c := testCache(t, srv, t.TempDir())
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	files["/p/sponsors.json"] = []byte(`{broken`)
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("broken manifest accepted")
	}
	delete(files, "/p/one.png")
	files["/p/sponsors.json"] = []byte(`{"slides":[{"image":"one.png","alt":"New"}]}`)
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("manifest with no loadable image accepted")
	}
	if len(c.slides) != 1 || c.slides[0].Alt != "One" {
		t.Errorf("last good copy lost: %+v", c.slides)
	}
}

func TestSponsorSizeLimits(t *testing.T) {
	files := map[string][]byte{
		"/p/sponsors.json": append([]byte(`{"slides":[],"pad":"`), append(bytes.Repeat([]byte("x"), sponsorMaxManifest), '"', '}')...),
	}
	srv := sponsorTestServer(t, files)
	c := testCache(t, srv, t.TempDir())
	if err := c.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("oversized manifest: %v", err)
	}
	files["/p/sponsors.json"] = []byte(`{"slides":[{"image":"big.png","alt":"Big"}]}`)
	files["/p/big.png"] = append(append([]byte{}, testPNG...), make([]byte, sponsorMaxImage)...)
	if err := c.refresh(context.Background()); err == nil {
		t.Error("oversized image accepted")
	}
}

func TestSponsorNilCacheServesEmpty(t *testing.T) {
	sponsor = nil
	mux := http.NewServeMux()
	registerSponsor(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sponsor/sponsors.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"slides":[]`) {
		t.Errorf("nil cache: %d %s", rec.Code, rec.Body.String())
	}
}
