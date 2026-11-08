package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"quill/internal/fakeapi"
)

func errorReply(status int, typ string, header http.Header) fakeapi.Reply {
	if header == nil {
		header = http.Header{}
	}
	header.Set("retry-after-ms", "1") // keep SDK retries fast
	return fakeapi.Reply{
		Status: status,
		Header: header,
		Body:   `{"type":"error","error":{"type":"` + typ + `","message":"msg ` + typ + `"},"request_id":"req_body"}`,
	}
}

func http529() fakeapi.Reply { return errorReply(529, "overloaded_error", nil) }

func TestClassify(t *testing.T) {
	cases := []struct {
		status    int
		typ       string
		header    http.Header
		kind      string
		retryable bool
	}{
		{401, "authentication_error", nil, "auth", false},
		{402, "billing_error", nil, "billing", false},
		{403, "permission_error", nil, "auth", false},
		{404, "not_found_error", nil, "model_unavailable", false},
		{400, "invalid_request_error", nil, "bad_request", false},
		{413, "request_too_large", nil, "bad_request", false},
		{422, "invalid_request_error", nil, "bad_request", false},
		{429, "rate_limit_error", http.Header{"Retry-After": {"7"}}, "rate_limited", true},
		{500, "api_error", nil, "overloaded", true},
		{503, "api_error", nil, "overloaded", true},
		{529, "overloaded_error", nil, "overloaded", true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.kind, tc.status), func(t *testing.T) {
			r := errorReply(tc.status, tc.typ, tc.header)
			// Enough copies for the first attempt plus SDK retries.
			srv := fakeapi.New(t, r, r, r)
			c := newClient(t, srv.URL())
			_, err := c.Step(context.Background(), simpleRequest())
			if err == nil {
				t.Fatal("expected error")
			}
			e := Classify(err)
			if e == nil {
				t.Fatalf("not classified: %v", err)
			}
			info := e.Info
			if info.Kind != tc.kind || info.Type != tc.typ || info.HTTPStatus != tc.status {
				t.Errorf("info %+v", info)
			}
			if info.Message != "msg "+tc.typ || info.RequestID != "req_body" || info.Retries != 2 {
				t.Errorf("info %+v", info)
			}
			if tc.status == 429 && info.RetryAfter != "7" {
				t.Errorf("retry_after %q", info.RetryAfter)
			}
			wantReqs := 1
			if tc.retryable {
				wantReqs = 3
			}
			if n := len(srv.Requests()); n != wantReqs {
				t.Errorf("%d requests, want %d", n, wantReqs)
			}
			if e.Error() == "" {
				t.Error("empty Error()")
			}
		})
	}
}

func TestClassifyRequestIDFromHeader(t *testing.T) {
	srv := fakeapi.New(t, fakeapi.Reply{
		Status: 401,
		Header: http.Header{"Request-Id": {"req_header"}},
		Body:   `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`,
	})
	c := newClient(t, srv.URL())
	_, err := c.Step(context.Background(), simpleRequest())
	if e := Classify(err); e == nil || e.Info.RequestID != "req_header" || e.Info.Kind != "auth" {
		t.Fatalf("got %+v", e)
	}
}

func TestClassifyNetwork(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := newClient(t, url, option.WithMaxRetries(0))
	_, err := c.Step(context.Background(), simpleRequest())
	e := Classify(err)
	if e == nil || e.Info.Kind != "network" || e.Info.HTTPStatus != 0 {
		t.Fatalf("got %+v (err %v)", e, err)
	}
}

func hangingServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server notices when the client goes away.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClassifyRequestTimeoutIsNetwork(t *testing.T) {
	c := newClient(t, hangingServer(t), option.WithMaxRetries(0), option.WithRequestTimeout(50*time.Millisecond))
	_, err := c.Step(context.Background(), simpleRequest())
	e := Classify(err)
	if e == nil || e.Info.Kind != "network" {
		t.Fatalf("got %+v (err %v)", e, err)
	}
}

func TestClassifyCallerContextNotClassified(t *testing.T) {
	url := hangingServer(t)
	c := newClient(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.Step(ctx, simpleRequest())
	if !errors.Is(err, context.Canceled) || Classify(err) != nil {
		t.Fatalf("cancel: err %v classified %+v", err, Classify(err))
	}

	ctx, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	_, err = c.Step(ctx, simpleRequest())
	if !errors.Is(err, context.DeadlineExceeded) || Classify(err) != nil {
		t.Fatalf("deadline: err %v classified %+v", err, Classify(err))
	}
}

func TestRefused(t *testing.T) {
	e := Refused()
	if e.Info.Kind != "refused" || e.Info.Type != "refusal" {
		t.Fatalf("got %+v", e.Info)
	}
}

func TestExhaustedScriptIs500(t *testing.T) {
	srv := fakeapi.New(t)
	c := newClient(t, srv.URL(), option.WithMaxRetries(0))
	_, err := c.Step(context.Background(), simpleRequest())
	e := Classify(err)
	if e == nil || e.Info.HTTPStatus != 500 || e.Info.Type != "api_error" {
		t.Fatalf("got %+v", e)
	}
}
