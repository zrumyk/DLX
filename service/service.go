/*
 * @Author: Vincent Yang
 * @Date: 2023-07-01 21:45:34
 * @LastEditors: Jason Lyu
 * @LastEditTime: 2025-04-08 13:45:00
 * @FilePath: /DLX/service/service.go
 * @Telegram: https://t.me/missuo
 * @GitHub: https://github.com/missuo
 *
 * Copyright © 2024 by Vincent, All Rights Reserved.
 */

package service

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/OwO-Network/DLX/translate"
)

const (
	// maxRequestBodyBytes bounds how much of a request body is buffered. The
	// translation limit is 1500 UTF-16 units, so any legitimate body is a few
	// kilobytes; 1 MiB leaves ample headroom for long arrays of tiny texts
	// while keeping a single request from allocating without bound.
	maxRequestBodyBytes = 1 << 20

	// maxMultipartMemory is the in-memory threshold handed to
	// ParseMultipartForm. The body itself is already capped well below it.
	maxMultipartMemory = 8 << 20

	// contextRawBody is where requestBodyLimit stores the buffered body so
	// handlers that need to sniff its shape (POST /v2/translate) can do so
	// without consuming it.
	contextRawBody = "dlx.rawRequestBody"
)

// credentialToken extracts the secret from an Authorization header. The scheme
// is case-insensitive (RFC 7235 §2.1) and whitespace between the scheme and the
// credentials may repeat, so it is split on runs of whitespace rather than on a
// single space. An empty result never authenticates.
func credentialToken(header string) string {
	fields := strings.Fields(header)
	if len(fields) != 2 {
		return ""
	}
	switch {
	case strings.EqualFold(fields[0], "Bearer"),
		strings.EqualFold(fields[0], "DeepL-Auth-Key"):
		return fields[1]
	}
	return ""
}

// tokenMatches compares a caller-supplied token with the configured one in
// constant time, so the comparison does not leak the token's prefix through
// response timing.
func tokenMatches(provided, want string) bool {
	if provided == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(want)) == 1
}

func authMiddleware(cfg *Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.Token == "" {
			c.Next()
			return
		}

		// Both places are accepted for compatibility with existing clients.
		// The query parameter is discouraged: it ends up in access logs,
		// browser history and Referer headers. Deployments that care can
		// reject `?token=` at the reverse proxy.
		headerToken := credentialToken(c.GetHeader("Authorization"))
		queryToken := c.Query("token")

		if !tokenMatches(headerToken, cfg.Token) && !tokenMatches(queryToken, cfg.Token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    http.StatusUnauthorized,
				"message": "Invalid access token",
			})
			return
		}

		c.Next()
	}
}

// requestBodyLimit buffers at most max bytes of the request body and restores it
// for the handlers. Anything larger is refused with 413 before a decoder or a
// translator ever sees it.
func requestBodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > max {
			abortBodyTooLarge(c, max)
			return
		}
		if c.Request.Body == nil {
			c.Next()
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, max+1))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Failed to read request body",
			})
			return
		}
		if int64(len(body)) > max {
			abortBodyTooLarge(c, max)
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Set(contextRawBody, body)
		c.Next()
	}
}

func abortBodyTooLarge(c *gin.Context, max int64) {
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
		"code":    http.StatusRequestEntityTooLarge,
		"message": fmt.Sprintf("request body exceeds %d bytes", max),
	})
}

type PayloadFree struct {
	TransText   string `json:"text"`
	SourceLang  string `json:"source_lang"`
	TargetLang  string `json:"target_lang"`
	TagHandling string `json:"tag_handling"`
}

type PayloadAPI struct {
	Text        []string `json:"text"`
	TargetLang  string   `json:"target_lang"`
	SourceLang  string   `json:"source_lang"`
	TagHandling string   `json:"tag_handling"`
}

// PayloadTranslate is the request body of POST /translate. Unlike PayloadFree
// (shared with the Pro endpoint), `text` accepts either a single string or an
// array of strings.
type PayloadTranslate struct {
	Text        PayloadText `json:"text"`
	SourceLang  string      `json:"source_lang"`
	TargetLang  string      `json:"target_lang"`
	TagHandling string      `json:"tag_handling"`
}

// PayloadText is the `text` field of POST /translate and POST /v2/translate, in
// either documented shape: a bare JSON string or an array of strings. Both are
// normalized to Texts, and Batch records which shape the caller used so the
// response can mirror it. Any other shape (a number, an object, an array
// holding non-strings) fails the payload decode, which the handler reports as
// 400 Invalid request payload; null names no text, like a body without the key.
type PayloadText struct {
	Texts []string
	Batch bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *PayloadText) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var texts []string
		if err := json.Unmarshal(trimmed, &texts); err != nil {
			return err
		}
		p.Texts, p.Batch = texts, true
		return nil
	}

	// A JSON null names no text: leave the zero value, so Texts stays nil.
	if string(trimmed) == "null" {
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	p.Texts, p.Batch = []string{text}, false
	return nil
}

// configureProxy points the process-wide HTTP transport at proxyURL. The
// default transport is cloned rather than replaced so its dial timeouts, TLS
// handshake timeout, connection limits and HTTP/2 support survive: a bare
// &http.Transport{Proxy: ...} silently disables all of them.
func configureProxy(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy url %q has no host", proxyURL)
	}

	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = http.ProxyURL(u)
	http.DefaultTransport = transport
	return nil
}

// requestDlSession returns the Pro credential for /v1/translate. Only the
// dl_session cookie is read; other cookies are never folded into the token.
// A dl_session cookie overrides the configured value. fromCookie reports where
// the value came from, because the legacy-cookie heuristic below must not be
// applied to an OAuth token supplied through -s/DL_SESSION.
func requestDlSession(c *gin.Context, fallback string) (session string, fromCookie bool) {
	raw := c.GetHeader("Cookie")
	if raw == "" {
		return fallback, false
	}

	if cookie, err := c.Request.Cookie("dl_session"); err == nil {
		if v := normalizeDlSession(cookie.Value); v != "" {
			return v, true
		}
	}

	// Compatibility: some callers put the bare session value in the Cookie
	// header instead of using `dl_session=<value>`. Accept that only when the
	// header carries no cookie syntax at all, so `foo=1; dl_session=x` can
	// never leak other cookies into the upstream token.
	if !strings.ContainsAny(raw, ";=") {
		if v := normalizeDlSession(strings.Trim(raw, `"`)); v != "" {
			// A bare value is what the legacy cookie path used to accept, so
			// treat it as cookie-derived.
			return v, true
		}
	}

	return fallback, false
}

func normalizeDlSession(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	return v
}

func Router(cfg *Config) *gin.Engine {
	// Set Proxy
	proxyURL := os.Getenv("PROXY")
	if proxyURL == "" {
		proxyURL = cfg.Proxy
	}
	if err := configureProxy(proxyURL); err != nil {
		log.Fatalf("Failed to configure proxy: %v", err)
	}

	if cfg.Token != "" {
		fmt.Println("Access token is set.")
	}

	r := gin.Default()
	r.Use(cors.Default())
	r.Use(requestBodyLimit(maxRequestBodyBytes))

	// Defining the root endpoint which returns the project details
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code":    http.StatusOK,
			"message": "DLX Translation API, Developed by sjlleo and missuo. Go to /translate with POST. https://github.com/OwO-Network/DLX",
		})
	})

	// Free API endpoint, No Pro Account required
	r.POST("/translate", authMiddleware(cfg), func(c *gin.Context) {
		req := PayloadTranslate{}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid request payload",
			})
			return
		}

		// A body without `text` never named a text to translate, which is a
		// payload error. `"text": []` names a list that happens to be empty,
		// and is answered with an empty list, so it is not caught here.
		if req.Text.Texts == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid request payload",
			})
			return
		}

		sourceLang := req.SourceLang
		targetLang := req.TargetLang
		texts := req.Text.Texts
		tagHandling := req.TagHandling

		proxyURL := cfg.Proxy

		if tagHandling != "" && tagHandling != "html" && tagHandling != "xml" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid tag_handling value. Allowed values are 'html' and 'xml'.",
			})
			return
		}

		result, err := translate.TranslateByDLX(sourceLang, targetLang, texts, tagHandling, proxyURL, "")
		if err != nil {
			log.Printf("Translation failed: %s", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    http.StatusInternalServerError,
				"message": "Translation failed",
			})
			return
		}

		if result.Code == http.StatusOK {
			response := gin.H{
				"code":         http.StatusOK,
				"id":           result.ID,
				"alternatives": result.Alternatives,
				"source_lang":  result.SourceLang,
				"target_lang":  result.TargetLang,
				"method":       result.Method,
			}
			// Mirror the request: a string in `text` answers with a string
			// in `data`, an array answers with a position-aligned array.
			if req.Text.Batch || len(result.Data) != 1 {
				response["data"] = result.Data
			} else {
				response["data"] = result.Data[0]
			}
			c.JSON(http.StatusOK, response)
		} else {
			c.JSON(result.Code, gin.H{
				"code":    result.Code,
				"message": result.Message,
			})

		}
	})

	// Pro API endpoint, Pro Account required
	r.POST("/v1/translate", authMiddleware(cfg), func(c *gin.Context) {
		req := PayloadFree{}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid request payload",
			})
			return
		}

		sourceLang := req.SourceLang
		targetLang := req.TargetLang
		translateText := req.TransText
		tagHandling := req.TagHandling
		proxyURL := cfg.Proxy

		if tagHandling != "" && tagHandling != "html" && tagHandling != "xml" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid tag_handling value. Allowed values are 'html' and 'xml'.",
			})
			return
		}

		dlSession, fromCookie := requestDlSession(c, cfg.DlSession)

		if dlSession == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"code":    http.StatusUnauthorized,
				"message": "No dl_session Found",
			})
			return
		}
		// A legacy free-account dl_session cookie contains a dot; an OAuth
		// access token (the -s/DL_SESSION value) may contain dots of its own,
		// so the heuristic only applies to cookie-derived values.
		if fromCookie && strings.Contains(dlSession, ".") {
			c.JSON(http.StatusUnauthorized, gin.H{
				"code":    http.StatusUnauthorized,
				"message": "Your account is not a Pro account. Please upgrade your account or switch to a different account.",
			})
			return
		}

		result, err := translate.TranslateByDLX(sourceLang, targetLang, []string{translateText}, tagHandling, proxyURL, dlSession)
		if err != nil {
			log.Printf("Translation failed: %s", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    http.StatusInternalServerError,
				"message": "Translation failed",
			})
			return
		}

		if result.Code == http.StatusOK {
			c.JSON(http.StatusOK, gin.H{
				"code":         http.StatusOK,
				"id":           result.ID,
				"data":         result.Data[0],
				"alternatives": result.Alternatives,
				"source_lang":  result.SourceLang,
				"target_lang":  result.TargetLang,
				"method":       result.Method,
			})
		} else {
			c.JSON(result.Code, gin.H{
				"code":    result.Code,
				"message": result.Message,
			})

		}
	})

	// Free API endpoint, Consistent with the official API format
	r.POST("/v2/translate", authMiddleware(cfg), func(c *gin.Context) {
		proxyURL := cfg.Proxy

		texts, sourceLang, targetLang, err := parseV2Request(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "Invalid request payload",
			})
			return
		}
		if len(texts) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "text is required",
			})
			return
		}
		if targetLang == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    http.StatusBadRequest,
				"message": "target_lang is required",
			})
			return
		}

		result, err := translate.TranslateByDLX(sourceLang, targetLang, texts, "", proxyURL, "")
		if err != nil {
			log.Printf("Translation failed: %s", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    http.StatusInternalServerError,
				"message": "Translation failed",
			})
			return
		}

		if result.Code == http.StatusOK {
			// Official v2 shape: one object per input text, position-aligned.
			// A batch is one upstream request, so it comes back in one piece.
			translations := make([]gin.H, 0, len(result.Data))
			for _, text := range result.Data {
				translations = append(translations, gin.H{
					"detected_source_language": result.SourceLang,
					"text":                     text,
				})
			}
			c.JSON(http.StatusOK, gin.H{"translations": translations})
		} else {
			c.JSON(result.Code, gin.H{
				"code":    result.Code,
				"message": result.Message,
			})
		}
	})

	// Catch-all route to handle undefined paths
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    http.StatusNotFound,
			"message": "Path not found",
		})
	})

	return r
}

// v2Payload is the JSON body of POST /v2/translate. `text` mirrors /translate:
// a bare string or an array of strings.
type v2Payload struct {
	Text       PayloadText `json:"text"`
	SourceLang string      `json:"source_lang"`
	TargetLang string      `json:"target_lang"`
}

// parseV2Request normalizes both documented encodings of /v2/translate:
//
//   - JSON, with `text` as a string or an array;
//   - form-urlencoded or multipart, with `text` repeated once per segment.
//
// The body was buffered by requestBodyLimit, so shallow content-type sniffing is
// safe: a body that starts with `{` is JSON even when the caller forgot the
// header, and a missing `content-type` on a form body still parses.
func parseV2Request(c *gin.Context) (texts []string, sourceLang, targetLang string, err error) {
	raw, _ := c.Get(contextRawBody)
	body, _ := raw.([]byte)

	if isJSONBody(c, body) {
		var p v2Payload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", "", err
		}
		return p.Text.Texts, p.SourceLang, p.TargetLang, nil
	}

	if strings.HasPrefix(strings.ToLower(c.ContentType()), "multipart/form-data") {
		if err := c.Request.ParseMultipartForm(maxMultipartMemory); err != nil {
			return nil, "", "", err
		}
		if c.Request.MultipartForm == nil {
			return nil, "", "", nil
		}
		return c.Request.MultipartForm.Value["text"],
			firstValue(c.Request.MultipartForm.Value["source_lang"]),
			firstValue(c.Request.MultipartForm.Value["target_lang"]), nil
	}

	if strings.EqualFold(c.ContentType(), "application/x-www-form-urlencoded") {
		if err := c.Request.ParseForm(); err != nil {
			return nil, "", "", err
		}
		return c.Request.PostForm["text"],
			c.Request.PostForm.Get("source_lang"),
			c.Request.PostForm.Get("target_lang"), nil
	}

	// net/http's ParseForm ignores the body when the media type is anything
	// but urlencoded or multipart, so a form body sent without a content type
	// is parsed here from the buffered bytes instead.
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, "", "", err
	}
	return values["text"], values.Get("source_lang"), values.Get("target_lang"), nil
}

func isJSONBody(c *gin.Context, body []byte) bool {
	ct := strings.ToLower(c.ContentType())
	if ct == "application/json" || strings.HasSuffix(ct, "+json") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
