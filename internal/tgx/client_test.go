package tgx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestErrorsNeverExposeBotToken(t *testing.T) {
	const token = "123456:fixture-secret"
	for _, tc := range []struct {
		name, body string
		network    bool
	}{
		{name: "gateway body", body: "upstream unavailable"},
		{name: "API rejection", body: `{"ok":false,"error_code":400,"description":"failure for /bot` + token + `/getMe"}`},
		{name: "network URL error", network: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
			defer endpoint.Close()
			c := New(endpoint.URL, token, "fixture")
			if tc.network {
				c.SetHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					return nil, &url.Error{Op: "request", URL: r.URL.String(), Err: context.Canceled}
				})})
			}
			err := c.TestConnectivity(context.Background())
			if err == nil {
				t.Fatal("Expected upstream error")
			}
			for _, secret := range []string{token, url.QueryEscape(token)} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("Bot token leaked")
				}
			}
			if tc.network && !errors.Is(err, context.Canceled) {
				t.Fatal("Redaction broke cancellation identity")
			}
		})
	}
}

func TestDeleteMessageOnlyTreatsMissingMessageAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		description string
		wantErr     bool
	}{
		{"Bad Request: message to delete not found", false},
		{"Bad Request: message can't be deleted", true},
		{"Bad Request: chat not found", true},
		{"Forbidden: bot is not an administrator", true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			c := New("http://fixture.invalid", "fixture-secret", "fixture")
			c.SetHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"` + tc.description + `"}`)), Request: r}, nil
			})})
			err := c.DeleteMessage(context.Background(), 1)
			if (err != nil) != tc.wantErr {
				t.Fatal("Deletion error classification", err)
			}
		})
	}
}

type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingBody) Close() error             { return b.err }

func TestDownloadRedactsNetworkAndStreamingErrors(t *testing.T) {
	const token = "123456:fixture-secret"
	for _, network := range []bool{true, false} {
		t.Run(map[bool]string{true: "network", false: "body"}[network], func(t *testing.T) {
			c := New("http://fixture.invalid", token, "fixture")
			c.SetHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"file_path":"fixture.bin"}}`)), Request: r}, nil
				}
				err := &url.Error{Op: "GET", URL: r.URL.String(), Err: context.Canceled}
				if network {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingBody{err: err}, Request: r}, nil
			})})
			rc, err := c.Download(context.Background(), "fixture-file")
			if network {
				if err == nil || strings.Contains(err.Error(), token) || !errors.Is(err, context.Canceled) {
					t.Fatal("Unsafe network error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(rc)
			if err == nil || strings.Contains(err.Error(), token) || !errors.Is(err, context.Canceled) {
				t.Fatal("Unsafe streaming error", err)
			}
			if err := rc.Close(); err == nil || strings.Contains(err.Error(), token) {
				t.Fatal("Unsafe close error", err)
			}
		})
	}
}
