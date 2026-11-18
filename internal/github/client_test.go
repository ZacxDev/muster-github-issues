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
