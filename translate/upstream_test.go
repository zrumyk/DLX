package translate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imroc/req/v3"
)

func useLoopbackUpstream(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := req.C()
	client.GetTransport().WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
		return req.HttpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.URL = endpoint
			r.Host = endpoint.Host
			return next.RoundTrip(r)
		})
	})
	oneshotClients.Store(server.URL, client)
	t.Cleanup(func() { oneshotClients.Delete(server.URL) })

	p := &iosAppVersionProvider
	p.mu.Lock()
	version, fetchedAt, attemptedAt := p.version, p.fetchedAt, p.attemptedAt
	p.version, p.fetchedAt = iosAppVersion, time.Now()
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.version, p.fetchedAt, p.attemptedAt = version, fetchedAt, attemptedAt
	})
	return server.URL
}

func TestUpstreamRateLimit(t *testing.T) {
	date := time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	for _, retryAfter := range []string{"180", date, ""} {
		t.Run("Retry-After="+retryAfter, func(t *testing.T) {
			proxy := useLoopbackUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("invalid gzip"))
			})
			result, err := TranslateByDLX("EN", "DE", []string{"Hello"}, "", proxy, "")
			if err != nil || result.Code != http.StatusTooManyRequests || result.RetryAfter != retryAfter {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestUpstreamRequestCancellation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	proxy := useLoopbackUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-time.After(5 * time.Second):
			t.Error("upstream did not observe cancellation")
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := TranslateByDLXContext(ctx, "EN", "DE", []string{"Hello"}, "", proxy, "")
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not return after cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not stop")
	}
}

func TestUpstreamMixedBatchAtUTF16Limit(t *testing.T) {
	atLimit := strings.Repeat("😀", 750)
	proxy := useLoopbackUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body oneshotRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if !reflect.DeepEqual(body.Text, []string{atLimit}) {
			t.Errorf("upstream received %d texts, want only the nonblank text", len(body.Text))
		}
		_, _ = w.Write([]byte(`{"translations":[{"text":"Hallo","detected_source_language":"EN"}]}`))
	})
	result, err := TranslateByDLX("EN", "DE", []string{"", atLimit, " \t"}, "", proxy, "")
	if err != nil || result.Code != http.StatusOK || !reflect.DeepEqual(result.Data, []string{"", "Hallo", " \t"}) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}
