package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster-github-issues/internal/github"
)

// A refusal that is not GitHub's JSON (a proxy's HTML error page) is logged,
// so its text is bounded.
func TestANonJSONErrorBodyIsCapped(t *testing.T) {
	page := "<html>" + strings.Repeat("<p>bad gateway</p>", 2000) + "</html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(page))
	}))
	defer srv.Close()
	err := github.New(srv.URL, "t").CheckRepo(context.Background(), github.Repo{Owner: "o", Name: "r"})
	e := github.AsError(err)
	if e == nil || e.Status != 502 {
		t.Fatalf("err = %v", err)
	}
	if len(e.Message) > 2<<10+16 || !strings.HasPrefix(e.Message, "<html><p>bad gateway</p>") || !strings.HasSuffix(e.Message, "…[cut]") {
		t.Fatalf("error text is %d bytes, want it cut to about 2 KiB: %.120s…", len(e.Message), e.Message)
	}
}

// GitHub's own JSON "message" is bounded the same way. An INVARIANT guard,
// not a regression test: the cap on this path was already in place, and this
// test passed before it was written; it exists to kill the mutant that drops
// the cap on the JSON path.
func TestALongJSONMessageIsCapped(t *testing.T) {
	msg := strings.Repeat("validation failed ", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"` + msg + `"}`))
	}))
	defer srv.Close()
	err := github.New(srv.URL, "t").CheckRepo(context.Background(), github.Repo{Owner: "o", Name: "r"})
	e := github.AsError(err)
	if e == nil || e.Status != 422 {
		t.Fatalf("err = %v", err)
	}
	if len(e.Message) > 2<<10+16 || !strings.HasPrefix(e.Message, "validation failed validation") || !strings.HasSuffix(e.Message, "…[cut]") {
		t.Fatalf("JSON message is %d bytes, want it cut to about 2 KiB", len(e.Message))
	}
}

// A body that is not UTF-8 at all is still cut to about 2 KiB, not emptied:
// one invalid byte used to make the cut strip the whole message.
func TestANonUTF8ErrorBodyIsCutNotEmptied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte("\xff\xfe" + strings.Repeat("gateway down ", 1000)))
	}))
	defer srv.Close()
	e := github.AsError(github.New(srv.URL, "t").CheckRepo(context.Background(), github.Repo{Owner: "o", Name: "r"}))
	if e == nil || len(e.Message) < 2<<10 || len(e.Message) > 2<<10+16 || !strings.Contains(e.Message, "gateway down") {
		t.Fatalf("a non-UTF-8 error body was cut to %d bytes: %.80q", len(e.Message), e)
	}
}
