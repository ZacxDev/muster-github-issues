package muster_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/mustertest"
)

func TestCheckBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://muster.example.com":   "https://muster.example.com",
		"https://muster.example.com/":  "https://muster.example.com",
		"http://localhost:8105":        "http://localhost:8105",
		"http://127.0.0.1:8105/":       "http://127.0.0.1:8105",
		"http://[::1]:8105":            "http://[::1]:8105",
		"http://muster.example.com":    "",
		"https://muster.example.com/x": "",
		"https://u@muster.example.com": "",
		"https://muster.example.com?q": "",
		"muster.example.com":           "",
	} {
		got, err := muster.CheckBaseURL(raw)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("CheckBaseURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := muster.CheckBaseURL("mpl_s3cretT0ken"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("the error echoes the value: %v", err)
	}
}

func TestNewRefusesATokenOfAnotherKind(t *testing.T) {
	if _, err := muster.New("https://muster.example.com", "mst_personal"); err == nil {
		t.Fatal("a non-plugin token was accepted")
	}
}

func TestPostTaskSendsTheTokenAndTheBody(t *testing.T) {
	var auth, body string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":4,"created":true,"commented":true,"routed":"chief"}`)
	}))
	defer s.Close()
	c, _ := muster.New(s.URL, mustertest.Token)
	ack, err := c.PostTask(context.Background(), muster.Task{Key: "github:o/r/1", Create: &muster.TaskCreate{Title: "t", Body: "b"},
		Comment: "[ev] new", RouteToChief: true, Notice: muster.NoticeAssigned})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+mustertest.Token {
		t.Fatalf("auth = %q", auth)
	}
	want := `{"key":"github:o/r/1","create":{"title":"t","body":"b"},"comment":"[ev] new","route_to_chief":true,"notice":"assigned"}`
	if body != want {
		t.Fatalf("body = %s\nwant   %s", body, want)
	}
	if ack.ID == nil || *ack.ID != 4 || !ack.Created || ack.RoutedTo() != "chief" {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestRoutedFalseIsNowhere(t *testing.T) {
	if got := (muster.TaskAck{Routed: []byte("false")}).RoutedTo(); got != "" {
		t.Fatalf("routed false = %q", got)
	}
}

func TestErrorsAreTypedWithRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status     int
		retryAfter string
		kind       muster.Kind
		wait       time.Duration
	}{
		{429, "7", muster.KindRateLimited, 7 * time.Second},
		{429, "", muster.KindRateLimited, 60 * time.Second},
		{503, "3", muster.KindServer, 3 * time.Second},
		{503, "", muster.KindServer, 0},
		{409, "", muster.KindConflict, 0},
		{400, "", muster.KindBadRequest, 0},
		{401, "", muster.KindAuth, 0},
		{404, "", muster.KindOff, 0},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, `{"error":"no"}`)
		}))
		c, _ := muster.New(s.URL, mustertest.Token)
		_, err := c.PostTask(context.Background(), muster.Task{Key: "alert:a", Comment: "x"})
		e := muster.AsError(err)
		if e == nil || e.Kind != tc.kind || e.RetryAfter != tc.wait || e.Message != "no" {
			t.Errorf("%d: err = %#v", tc.status, err)
		}
		s.Close()
	}
}

func TestANetworkErrorNamesTheSchemeAndHostOnly(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	base := s.URL
	s.Close()
	c, _ := muster.New(base, mustertest.Token)
	_, err := c.PostTask(context.Background(), muster.Task{Key: "alert:a", Comment: "x"})
	if e := muster.AsError(err); e == nil || e.Kind != muster.KindNetwork || strings.Contains(err.Error(), "/plugin/v1") {
		t.Fatalf("err = %v", err)
	}
}

// A copy-task request has no event_id field at all: muster answers a create or
// comment that carries one 400 "event_id is only for reference".
func TestATaskMarshalsWithoutEventID(t *testing.T) {
	b, err := json.Marshal(muster.Task{Key: "alert:a", Create: &muster.TaskCreate{Title: "t", Body: "b"}, Comment: "[e] x", RouteToChief: true, Notice: muster.NoticeAssigned})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "event_id") || strings.Contains(string(b), "reference") {
		t.Fatalf("a copy task carries a reference field: %s", b)
	}
}

// A refusal's text is logged, so it is bounded: muster's own JSON "error" and
// a body that is not JSON (a proxy's HTML page) alike.
func TestARefusalsTextIsCapped(t *testing.T) {
	long := strings.Repeat("refused ", 1000)
	for name, body := range map[string]string{
		"json":      `{"error":"` + long + `"}`,
		"not json":  "<html>" + long + "</html>",
		"not utf-8": "\xff\xfe" + long, // one invalid byte must not empty the cut
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			io.WriteString(w, body)
		}))
		c, _ := muster.New(srv.URL, mustertest.Token)
		_, err := c.PostTask(context.Background(), muster.Task{Key: "x:y", Comment: "c"})
		srv.Close()
		e := muster.AsError(err)
		if e == nil || e.Status != 400 {
			t.Fatalf("%s: err = %v", name, err)
		}
		if len(e.Message) > 2<<10+16 || !strings.HasSuffix(e.Message, "…[cut]") || !strings.Contains(e.Message, "refused refused") {
			t.Errorf("%s: error text is %d bytes, want it cut to about 2 KiB", name, len(e.Message))
		}
	}
}
