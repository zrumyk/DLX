package service

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/OwO-Network/DLX/translate"
)

func TestTranslationRoutesForwardRetryAfter(t *testing.T) {
	for _, retryAfter := range []string{"180", "Thu, 26 Nov 2026 22:00:00 GMT", ""} {
		router := newRouter(&Config{Token: "fixture-token", DlSession: "fixture-session"},
			func(_ context.Context, _, _ string, _ []string, _, _, _ string) (translate.DLXTranslationResult, error) {
				return translate.DLXTranslationResult{Code: http.StatusTooManyRequests, RetryAfter: retryAfter}, nil
			})
		for _, route := range []string{"/translate", "/v1/translate", "/v2/translate"} {
			request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"text":"Hello","target_lang":"DE"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer fixture-token")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != retryAfter {
				t.Errorf("%s status = %d, Retry-After = %q, want 429 and %q", route, recorder.Code, recorder.Header().Get("Retry-After"), retryAfter)
			}
		}
	}
}

func TestServiceAccessLogOmitsQueryCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := gin.DefaultWriter
	gin.DefaultWriter = &output
	t.Cleanup(func() { gin.DefaultWriter = previous })

	router := newRouter(&Config{Token: "synthetic-secret"}, translate.TranslateByDLXContext)
	request := httptest.NewRequest(http.MethodPost, "/translate?token=synthetic-secret&other=synthetic-query",
		strings.NewReader(`{"text":[],"target_lang":"DE"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	logged := output.String()
	if recorder.Code != http.StatusOK || !strings.Contains(logged, `POST "/translate" 200`) ||
		strings.Contains(logged, "synthetic-secret") || strings.Contains(logged, "synthetic-query") {
		t.Fatalf("status = %d, access log = %q", recorder.Code, logged)
	}
}

func TestProxyErrorsOmitCredentials(t *testing.T) {
	for _, proxy := range []string{
		"http://user:synthetic-secret@127.0.0.1:%zz",
		"ftp://user:synthetic-secret@127.0.0.1:8080",
		"http://user:synthetic-secret@",
	} {
		cfg := Config{Proxy: proxy, Port: 1188}
		for _, err := range []error{cfg.validate(), configureProxy(proxy)} {
			if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
				t.Errorf("expected a redacted configuration error, got %v", err)
			}
		}
	}
}

// The `text` field of POST /translate accepts either shape; decoding uses the
// same encoding/json decoder gin binds request bodies with.
func TestPayloadTextUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantTexts []string
		wantBatch bool
		wantErr   bool
	}{
		{name: "string", payload: `{"text":"Hello"}`, wantTexts: []string{"Hello"}},
		{name: "empty string", payload: `{"text":""}`, wantTexts: []string{""}},
		{name: "array", payload: `{"text":["Hello","Good morning"]}`, wantTexts: []string{"Hello", "Good morning"}, wantBatch: true},
		{name: "single-element array", payload: `{"text":["Hello"]}`, wantTexts: []string{"Hello"}, wantBatch: true},
		{name: "empty array", payload: `{"text":[]}`, wantTexts: []string{}, wantBatch: true},
		{name: "null", payload: `{"text":null}`, wantTexts: nil},
		{name: "no text field", payload: `{}`, wantTexts: nil},
		{name: "number", payload: `{"text":1}`, wantErr: true},
		{name: "object", payload: `{"text":{}}`, wantErr: true},
		{name: "mixed array", payload: `{"text":["Hello",1]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req PayloadTranslate
			err := json.NewDecoder(strings.NewReader(tt.payload)).Decode(&req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Decode(%s) error = %v, wantErr %v", tt.payload, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if req.Text.Batch != tt.wantBatch {
				t.Errorf("Batch = %v, want %v", req.Text.Batch, tt.wantBatch)
			}
			if !reflect.DeepEqual(req.Text.Texts, tt.wantTexts) {
				t.Errorf("Texts = %q, want %q", req.Text.Texts, tt.wantTexts)
			}
		})
	}
}

// POST /translate mirrors the shape it was given, and no shape that carries
// text is an error any more — an empty text is answered with itself and an
// empty array with an empty array. Every case here is answered (or refused)
// before a translation is attempted, so the test never reaches DeepL.
func TestTranslateHandlerAnswersEmptyText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := Router(&Config{})

	tests := []struct {
		name       string
		payload    string
		wantCode   int
		wantInBody string
	}{
		{"empty array", `{"text":[],"target_lang":"EN"}`, http.StatusOK, `"data":[]`},
		{"empty string", `{"text":"","target_lang":"EN"}`, http.StatusOK, `"data":""`},
		{"null", `{"text":null,"target_lang":"EN"}`, http.StatusBadRequest, `Invalid request payload`},
		{"array of empty", `{"text":[""],"target_lang":"EN"}`, http.StatusOK, `"data":[""]`},
		{"no text field", `{"target_lang":"EN"}`, http.StatusBadRequest, `Invalid request payload`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/translate", strings.NewReader(tt.payload))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantCode {
				t.Fatalf("POST /translate %s = %d %s, want %d", tt.payload, recorder.Code, recorder.Body.String(), tt.wantCode)
			}
			if !strings.Contains(recorder.Body.String(), tt.wantInBody) {
				t.Fatalf("POST /translate %s body = %s, want it to contain %s", tt.payload, recorder.Body.String(), tt.wantInBody)
			}
		})
	}
}

func TestCredentialToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer secret", "secret"},
		{"bearer secret", "secret"},
		{"BEARER secret", "secret"},
		{"DeepL-Auth-Key secret", "secret"},
		{"deepl-auth-key secret", "secret"},
		{"Bearer   secret", "secret"}, // runs of whitespace
		{"Bearer secret extra", ""},
		{"Basic secret", ""},
		{"secret", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := credentialToken(tt.header); got != tt.want {
			t.Errorf("credentialToken(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/x", authMiddleware(&Config{Token: "secret"}), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name   string
		header string
		query  string
		want   int
	}{
		{"no credentials", "", "", http.StatusUnauthorized},
		{"bearer", "Bearer secret", "", http.StatusNoContent},
		{"lowercase scheme", "bearer secret", "", http.StatusNoContent},
		{"deepl auth key", "DeepL-Auth-Key secret", "", http.StatusNoContent},
		{"double space", "Bearer  secret", "", http.StatusNoContent},
		{"wrong token", "Bearer nope", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic secret", "", http.StatusUnauthorized},
		{"query token", "", "?token=secret", http.StatusNoContent},
		{"wrong query token", "", "?token=nope", http.StatusUnauthorized},
		{"empty query token", "", "?token=", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/x"+tt.query, nil)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.want, recorder.Body.String())
			}
		})
	}
}

// Only dl_session may become the upstream credential; other cookies must never
// be folded into the token (the pre-audit handler sent "Bearer foo=1; fakepro").
func TestRequestDlSession(t *testing.T) {
	tests := []struct {
		name       string
		cookie     string
		want       string
		wantCookie bool
	}{
		{"no header", "", "fallback", false},
		{"plain cookie", "dl_session=abc", "abc", true},
		{"other cookies are ignored", "foo=1; dl_session=abc; bar=2", "abc", true},
		{"quoted value", `dl_session="abc"`, "abc", true},
		{"bare value for compatibility", "abc", "abc", true},
		{"cookie that is not dl_session", "foo=1", "fallback", false},
		{"empty dl_session", "dl_session=", "fallback", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			request := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			if tt.cookie != "" {
				request.Header.Set("Cookie", tt.cookie)
			}
			c.Request = request

			got, fromCookie := requestDlSession(c, "fallback")
			if got != tt.want || fromCookie != tt.wantCookie {
				t.Fatalf("requestDlSession() = (%q, %v), want (%q, %v)", got, fromCookie, tt.want, tt.wantCookie)
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := Router(&Config{})

	body := `{"text":"` + strings.Repeat("a", maxRequestBodyBytes) + `","target_lang":"EN"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/translate", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":413`) {
		t.Fatalf("body = %s, want a 413 payload", recorder.Body.String())
	}
}

func newV2Context(t *testing.T, contentType string, body []byte) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v2/translate", bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	c.Request = request
	// requestBodyLimit does this in the real pipeline.
	c.Set(contextRawBody, body)
	return c
}

func TestParseV2Request(t *testing.T) {
	var multipartBody bytes.Buffer
	mw := multipart.NewWriter(&multipartBody)
	for _, kv := range [][2]string{
		{"text", "Hello"}, {"text", "Good morning"}, {"source_lang", "EN"}, {"target_lang", "UK"},
	} {
		_ = mw.WriteField(kv[0], kv[1])
	}
	_ = mw.Close()

	tests := []struct {
		name        string
		contentType string
		body        string
		wantTexts   []string
		wantSource  string
		wantTarget  string
		wantErr     bool
	}{
		{
			name: "json string", contentType: "application/json",
			body:      `{"text":"Hello","source_lang":"EN","target_lang":"UK"}`,
			wantTexts: []string{"Hello"}, wantSource: "EN", wantTarget: "UK",
		},
		{
			name: "json array", contentType: "application/json",
			body:      `{"text":["Hello","Good morning"],"source_lang":"EN","target_lang":"UK"}`,
			wantTexts: []string{"Hello", "Good morning"}, wantSource: "EN", wantTarget: "UK",
		},
		{
			name:      "json without content type",
			body:      `{"text":["Hello","Good morning"],"target_lang":"UK"}`,
			wantTexts: []string{"Hello", "Good morning"}, wantTarget: "UK",
		},
		{
			name: "json null", contentType: "application/json",
			body:       `{"text":null,"target_lang":"UK"}`,
			wantTarget: "UK",
		},
		{
			name: "form repeats", contentType: "application/x-www-form-urlencoded",
			body:      "text=Hello&text=Good+morning&source_lang=EN&target_lang=UK",
			wantTexts: []string{"Hello", "Good morning"}, wantSource: "EN", wantTarget: "UK",
		},
		{
			name:      "form without content type",
			body:      "text=Hello&text=Good+morning&target_lang=UK",
			wantTexts: []string{"Hello", "Good morning"}, wantTarget: "UK",
		},
		{
			name: "multipart", contentType: mw.FormDataContentType(), body: multipartBody.String(),
			wantTexts: []string{"Hello", "Good morning"}, wantSource: "EN", wantTarget: "UK",
		},
		{
			name: "malformed json", contentType: "application/json", body: `{"text":`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newV2Context(t, tt.contentType, []byte(tt.body))
			texts, source, target, err := parseV2Request(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseV2Request() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(texts, tt.wantTexts) || source != tt.wantSource || target != tt.wantTarget {
				t.Fatalf("parseV2Request() = (%q, %q, %q), want (%q, %q, %q)",
					texts, source, target, tt.wantTexts, tt.wantSource, tt.wantTarget)
			}
		})
	}
}

// /v2/translate answers one translation object per requested text, and never
// reaches DeepL for blank texts.
func TestV2HandlerBlankBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := Router(&Config{})

	tests := []struct {
		name       string
		payload    string
		wantCode   int
		wantInBody string
	}{
		{
			name:       "blank batch keeps positions",
			payload:    `{"text":["","  "],"source_lang":"EN","target_lang":"UK"}`,
			wantCode:   http.StatusOK,
			wantInBody: `"translations":[{"detected_source_language":"EN","text":""},{"detected_source_language":"EN","text":"  "}]`,
		},
		{
			name:       "single empty string",
			payload:    `{"text":"","source_lang":"EN","target_lang":"UK"}`,
			wantCode:   http.StatusOK,
			wantInBody: `"translations":[{"detected_source_language":"EN","text":""}]`,
		},
		{
			name:       "empty array",
			payload:    `{"text":[],"target_lang":"UK"}`,
			wantCode:   http.StatusBadRequest,
			wantInBody: `text is required`,
		},
		{
			name:       "missing text",
			payload:    `{"target_lang":"UK"}`,
			wantCode:   http.StatusBadRequest,
			wantInBody: `text is required`,
		},
		{
			name:       "missing target",
			payload:    `{"text":"Hello"}`,
			wantCode:   http.StatusBadRequest,
			wantInBody: `target_lang is required`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/translate", strings.NewReader(tt.payload))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantCode, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tt.wantInBody) {
				t.Fatalf("body = %s, want it to contain %s", recorder.Body.String(), tt.wantInBody)
			}
		})
	}
}
