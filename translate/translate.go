/*
 * @Author: Vincent Young
 * @Date: 2024-09-16 11:59:24
 * @LastEditors: Vincent Yang
 * @LastEditTime: 2026-08-04 00:00:00
 * @FilePath: /DLX/translate/translate.go
 * @Telegram: https://t.me/missuo
 * @GitHub: https://github.com/missuo
 *
 * Copyright © 2024 by Vincent, All Rights Reserved.
 */

package translate

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/imroc/req/v3"
	"github.com/imroc/req/v3/http2"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

// DeepL's interactive clients (web, Chrome extension, and the official iOS
// app) all share the same stateless "oneshot" translate endpoint. The
// legacy LMT_handle_texts backend on www2.deepl.com rate-limits anonymous
// traffic hard; oneshot lives on a separate pool and accepts the literal
// header `Authorization: None` for free requests.
//
// Request shape reverse-engineered from the DeepL iOS client (bundle
// com.linguee.DeepLMobileTranslator, IPA Info.plist + ItaClient.framework).
// The advertised app version is resolved at runtime from the App Store
// (appversion.go) so a DeepL release cannot silently invalidate the profile;
// iosAppVersion below is only the fallback.
//
//	Transport
//	  DeepL answers 429 on the iOS ClientHello (utls HelloIOS) regardless of
//	  host or caller IP. The client therefore presents a desktop Chrome
//	  profile — TLS ClientHello, HTTP/2 SETTINGS and header order, sec-ch-ua*
//	  and sec-fetch-* — which is what deepl.com/translator itself sends.
//
//	Free URL   → https://oneshot-free.www.deepl.com/v1/translate
//	Pro URL    → https://oneshot. + <cell> + .pro.deepl.com/v1/translate
//	             (we keep oneshot-pro.www as the free-tier Pro fallback)
//
//	Headers (free path, an XHR from a deepl.com tab):
//	  Authorization: None          (ItaClient.LoginNone)
//	  User-Agent                   (desktop Chrome, matching the TLS profile)
//	  sec-ch-ua / sec-fetch-*      (consistent with that Chrome build)
//	  Origin / Referer             (an XHR from a deepl.com tab)
//
//	Body (free path):
//	  text[], target_lang, source_lang?, usage_type
//	  usage_type ∈ {translate, ocr, voiceforconversations}
//	  The iOS app_information block is omitted: it would be a mismatched-client
//	  tell next to a browser fingerprint. The Pro path still sends it.
//
// DeepL rate-limits clients whose TLS, UA and headers tell different stories.
// Keep every layer describing the same Chrome build.
const (
	oneshotFreeEndpoint = "https://oneshot-free.www.deepl.com/v1/translate"
	oneshotProEndpoint  = "https://oneshot-pro.www.deepl.com/v1/translate"

	// Fallback DeepL iOS version, used only when the runtime App Store
	// lookup fails (appversion.go). Keep it tracking DeepL's current App Store
	// release: a stale version is answered with HTTP 429 on every request
	// ("your IP has been blocked by DeepL temporarily"), from any IP. The build
	// number stays pinned because Apple's lookup API does not expose
	// CFBundleVersion.
	//
	// Last refreshed against the US App Store on 2026-11-26 (26.59).
	iosAppVersion = "26.59"
	iosAppBuild   = "5443737"

	// Reported OS version for app_information.os_version + x-app-os-version.
	// IPA is built against iphoneos26.5 (DTPlatformVersion). Must be a real
	// shipping major — a future value (e.g. 27.0) combined with an iOS TLS
	// fingerprint is rejected with HTTP 429 and can temp-ban the IP.
	iosOSVersion = "26.0"

	// oneshot enforces a 1500-character hard cap on the total length of
	// the `text` array for anonymous traffic (same limit the Chrome
	// extension documents as G.notLoggedIn). A character there is a UTF-16
	// code unit — what JavaScript's String.prototype.length and Kotlin's
	// String.length report — so an astral character counts as two. Bail
	// early to spare the upstream and give the caller a faster error.
	maxFreeTextLength = 1500

	// oneshotTimeout caps how long we wait on a single translate request.
	oneshotTimeout = 20 * time.Second

	// warmupTimeout caps the initial GET to www.deepl.com that seeds the
	// cookie jar. Cookies are best-effort; skip a slow warmup rather than
	// block the first translation.
	warmupTimeout = 5 * time.Second
)

// chromeUserAgent / chromeSecCHUA must stay one consistent Chrome build so the
// User-Agent, the client hints and the TLS fingerprint all describe the same
// browser. Do not mix versions across the three.
const chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const chromeSecCHUA = `"Chromium";v="131", "Not_A Brand";v="24", "Google Chrome";v="131"`

// instanceID mirrors the UUID the iOS app persists for analytics /
// app_information.instance_id: stable for the life of the process, reused on
// every Pro request. Rotating per-request is a stronger bot signal than
// reusing one.
var instanceID = newInstanceID()

// A real iOS URLSession inherits whatever cookies the app has on
// .deepl.com. A cold visit to www.deepl.com sets userCountry=<iso2> and
// verifiedBot=false. Share a process-wide jar so every oneshot POST
// carries whatever the warmup GET picked up.
var (
	cookieJar     http.CookieJar
	cookieJarOnce sync.Once
	cookieWarmer  sync.Once
)

// oneshotClients caches one req.Client per proxy URL so all translate
// calls share the underlying TCP / TLS / HTTP/2 connection pool.
var oneshotClients sync.Map // map[string]*req.Client

func sharedCookieJar() http.CookieJar {
	cookieJarOnce.Do(func() {
		j, _ := cookiejar.New(nil)
		cookieJar = j
	})
	return cookieJar
}

// warmCookies primes the shared jar by GETting www.deepl.com once.
// The Set-Cookie response lands on .deepl.com (eTLD+1 of oneshot-free),
// so subsequent POSTs carry those cookies automatically.
func warmCookies(client *req.Client) {
	cookieWarmer.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), warmupTimeout)
		defer cancel()
		_, _ = client.R().SetContext(ctx).Get("https://www.deepl.com/translator")
	})
}

func newInstanceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // RFC 4122 v4
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[0:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

// Language code tables mirror ItaClient.OutputLanguage / InputLanguage
// (regional cases enUs, enGb, frCa, deCh, ptPt, ptBr, es419, zhHant, …)
// plus the full target-capable set the oneshot endpoint accepts.
//
// Keys are the uppercase forms callers pass; values are the lowercase
// BCP-47-ish forms oneshot expects ("de", "en-US", "zh-Hans", ...).
//
// EN and PT are intentionally absent as bare target codes — DeepL
// deprecated them in favour of EN-US/EN-GB and PT-BR/PT-PT. We accept
// EN/PT as a backward-compat convenience and resolve them to the
// regional default (en-US, pt-BR).
var targetLangMap = map[string]string{
	"AR": "ar", "BG": "bg", "CS": "cs", "DA": "da", "DE": "de", "DE-CH": "de-CH",
	"EL":    "el",
	"EN-GB": "en-GB", "EN-US": "en-US",
	"ES": "es", "ES-419": "es-419", "ET": "et", "FI": "fi", "FR": "fr", "FR-CA": "fr-CA",
	"HE": "he", "HU": "hu", "ID": "id", "IT": "it", "JA": "ja", "KO": "ko",
	"LT": "lt", "LV": "lv", "NB": "nb", "NL": "nl", "PL": "pl",
	"PT-BR": "pt-BR", "PT-PT": "pt-PT",
	"RO": "ro", "RU": "ru", "SK": "sk", "SL": "sl", "SV": "sv",
	"TR": "tr", "UK": "uk", "VI": "vi",
	"ZH": "zh-Hans", "ZH-HANS": "zh-Hans", "ZH-HANT": "zh-Hant",
	// Convenience aliases for legacy callers.
	"EN": "en-US",
	"PT": "pt-BR",
}

// sourceLangMap is what the API accepts as `source_lang`. It is a
// superset of targetLangMap: EN and PT are first-class source codes
// mapping to the generic "en"/"pt", and the Chinese codes all map to the
// generic "zh".
var sourceLangMap = func() map[string]string {
	m := make(map[string]string, len(targetLangMap)+2)
	for k, v := range targetLangMap {
		m[k] = v
	}
	m["EN"] = "en"
	m["PT"] = "pt"
	// DeepL's source codes are the generic language forms. zh-Hans/zh-Hant
	// are target-only script variants: sent as a source hint they are
	// silently ignored, so ambiguous mixed-script Chinese fell back to
	// auto-detection and came back untranslated (#239).
	m["ZH"] = "zh"
	m["ZH-HANS"] = "zh"
	m["ZH-HANT"] = "zh"
	return m
}()

// resolveTargetLang validates and normalizes a user-supplied target
// language code. Returns "" and a non-nil error if the code is empty,
// "auto", or otherwise not in the supported set.
func resolveTargetLang(code string) (string, error) {
	if code == "" {
		return "", fmt.Errorf("target_lang is required")
	}
	if strings.EqualFold(code, "auto") {
		return "", fmt.Errorf("target_lang cannot be \"auto\"; pick one of: %s", supportedTargetLangsList())
	}
	if v, ok := targetLangMap[strings.ToUpper(code)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("unsupported target_lang %q; valid codes: %s", code, supportedTargetLangsList())
}

// resolveSourceLang validates and normalizes a user-supplied source
// language code. An empty string or "auto" is allowed and returns
// ("", nil) so the caller omits source_lang and lets the server
// autodetect.
func resolveSourceLang(code string) (string, error) {
	if code == "" || strings.EqualFold(code, "auto") {
		return "", nil
	}
	if v, ok := sourceLangMap[strings.ToUpper(code)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("unsupported source_lang %q; valid codes: %s (or \"auto\")", code, supportedSourceLangsList())
}

// supportedTargetLangsList / supportedSourceLangsList return a sorted,
// comma-separated rendering of the supported codes for use in error
// messages. Cached at first call.
var (
	targetLangsListOnce sync.Once
	targetLangsList     string
	sourceLangsListOnce sync.Once
	sourceLangsList     string
)

func supportedTargetLangsList() string {
	targetLangsListOnce.Do(func() {
		targetLangsList = sortedKeys(targetLangMap)
	})
	return targetLangsList
}

func supportedSourceLangsList() string {
	sourceLangsListOnce.Do(func() {
		sourceLangsList = sortedKeys(sourceLangMap)
	})
	return sourceLangsList
}

func sortedKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// appInformation matches ItaClient.AppInformation (os, os_version,
// app_version, app_build, instance_id) as serialized by the iOS client.
type appInformation struct {
	OS         string `json:"os"`
	OSVersion  string `json:"os_version"`
	AppVersion string `json:"app_version"`
	AppBuild   string `json:"app_build"`
	InstanceID string `json:"instance_id"`
}

// oneshotRequest mirrors the body assembled by the iOS OneShotTranslator
// / ItaClient oneshot path. Field order matches the app's serialization
// so the JSON is byte-stable (encoding/json honours struct field order).
// AppInformation is omitted on the free path, which presents a browser
// fingerprint instead of the iOS app profile.
type oneshotRequest struct {
	Text           []string        `json:"text"`
	TargetLang     string          `json:"target_lang"`
	SourceLang     string          `json:"source_lang,omitempty"`
	UsageType      string          `json:"usage_type"`
	AppInformation *appInformation `json:"app_information,omitempty"`
}

// getOneshotClient returns a process-wide cached client for the given
// proxy URL, creating it on first use. Sharing the client across
// requests keeps the TLS / HTTP/2 connection in the pool.
func getOneshotClient(proxyURL string) (*req.Client, error) {
	if c, ok := oneshotClients.Load(proxyURL); ok {
		return c.(*req.Client), nil
	}
	c, err := newOneshotClient(proxyURL)
	if err != nil {
		return nil, err
	}
	if actual, loaded := oneshotClients.LoadOrStore(proxyURL, c); loaded {
		return actual.(*req.Client), nil
	}
	go warmCookies(c)
	return c, nil
}

func newOneshotClient(proxyURL string) (*req.Client, error) {
	// DeepL answers 429 on the iOS ClientHello (utls HelloIOS) regardless of
	// endpoint or IP. The profile therefore rides a full desktop-Chrome
	// fingerprint: TLS ClientHello, HTTP/2 SETTINGS and header order, and the
	// matching sec-ch-ua / sec-fetch headers, so the request is
	// indistinguishable from an XHR fired by a real Chrome tab on deepl.com.
	client := req.C().
		SetTLSFingerprintChrome().
		SetCookieJar(sharedCookieJar()).
		SetTimeout(oneshotTimeout).
		SetUserAgent(chromeUserAgent).
		SetCommonHeader("Accept", "*/*").
		SetCommonHeader("Accept-Encoding", "gzip, deflate, br, zstd").
		SetCommonHeader("Accept-Language", "en-US,en;q=0.9").
		SetCommonHeader("sec-ch-ua", chromeSecCHUA).
		SetCommonHeader("sec-ch-ua-mobile", "?0").
		SetCommonHeader("sec-ch-ua-platform", `"Windows"`).
		// Chrome's HTTP/2 ClientHello: SETTINGS order/values, pseudo-header
		// order, header order and connection window. Together these make the
		// Akamai h2 fingerprint `1:65536;2:0;4:6291456;6:262144`.
		SetCommonPseudoHeaderOder(":method", ":authority", ":scheme", ":path").
		SetCommonHeaderOrder(
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"user-agent",
			"content-type",
			"authorization",
			"accept",
			"origin",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
		).
		SetHTTP2SettingsFrame(
			http2.Setting{ID: http2.SettingHeaderTableSize, Val: 65536},
			http2.Setting{ID: http2.SettingEnablePush, Val: 0},
			http2.Setting{ID: http2.SettingInitialWindowSize, Val: 6291456},
			http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: 262144},
		).
		SetHTTP2ConnectionFlow(15663105)

	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		client.SetProxyURL(u.String())
	}
	return client, nil
}

type oneshotResponse struct {
	body       gjson.Result
	status     int
	retryAfter string
}

// callOneshot POSTs to the oneshot endpoint and returns its status and data.
// For anonymous traffic bearerToken is empty and we send the literal
// header `Authorization: None` — matching ItaClient.LoginNone. Omitting
// that header puts the request on a different server-side auth branch.
func callOneshot(ctx context.Context, endpoint string, body []byte, bearerToken, proxyURL string) (oneshotResponse, error) {
	client, err := getOneshotClient(proxyURL)
	if err != nil {
		return oneshotResponse{}, err
	}

	// LoginNone → literal "None"; LoginPro/Free → "Bearer <access_token>".
	authValue := "None"
	if bearerToken != "" {
		authValue = "Bearer " + bearerToken
	}

	resp, err := client.R().
		SetContext(ctx).
		DisableAutoReadResponse().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", authValue).
		SetHeader("User-Agent", chromeUserAgent).
		// A cross-origin XHR from a deepl.com tab: same-site, CORS mode, no
		// navigation. The x-app-* iOS headers are gone — they were the tell.
		SetHeader("Origin", "https://www.deepl.com").
		SetHeader("Referer", "https://www.deepl.com/en/translator").
		SetHeader("sec-fetch-site", "same-site").
		SetHeader("sec-fetch-mode", "cors").
		SetHeader("sec-fetch-dest", "empty").
		SetBodyBytes(body). // pins Content-Length; an io.Reader would
		// force Transfer-Encoding: chunked, which URLSession JSON bodies
		// never emit.
		Post(endpoint)
	if err != nil {
		return oneshotResponse{}, err
	}
	defer resp.Body.Close()

	response := oneshotResponse{status: resp.StatusCode}
	if response.status == http.StatusTooManyRequests {
		response.retryAfter = resp.Header.Get("Retry-After")
		return response, nil
	}

	// Once we set Accept-Encoding ourselves, Go's HTTP stack stops
	// transparently decompressing, so handle gzip/deflate/br by hand.
	var reader io.Reader = resp.Body
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return response, fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		reader = gr
	case "deflate":
		reader = flate.NewReader(resp.Body)
	case "br":
		reader = brotli.NewReader(resp.Body)
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return response, fmt.Errorf("zstd reader: %w", err)
		}
		defer zr.Close()
		reader = zr
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return response, fmt.Errorf("read response body: %w", err)
	}
	response.body = gjson.ParseBytes(raw)
	return response, nil
}

// textLength returns the length of text in UTF-16 code units — the unit
// oneshot's anonymous 1500-character cap counts, and the one the DeepL clients
// this profile mirrors report (the iOS app is Kotlin, the extension is
// JavaScript). An astral character is a surrogate pair and so counts as two,
// where a rune count scores it one and lets an oversized request travel
// upstream to be rejected there as a bare 400.
//
// Invalid UTF-8 needs no special case: the request body is decoded with
// encoding/json before this runs, which replaces every invalid byte with
// U+FFFD, and each replacement is one unit either way.
func textLength(text string) int {
	n := 0
	for _, r := range text {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// textsToTranslate returns the positions of the texts that carry something to
// translate and their total length in UTF-16 code units. A blank text — empty,
// or nothing but whitespace — carries nothing to translate, so it is not sent:
// its own text is the answer, which keeps a segmented document position-aligned
// and lets an empty line pass through instead of failing the request.
func textsToTranslate(texts []string) (positions []int, total int) {
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		positions = append(positions, i)
		total += textLength(text)
	}
	return positions, total
}

// mergeTranslations puts the translations back at the positions they were asked
// for, leaving every text that was not sent — a blank one — exactly as it
// arrived.
func mergeTranslations(texts []string, positions []int, translations []string) []string {
	merged := make([]string, len(texts))
	copy(merged, texts)
	for i, position := range positions {
		merged[position] = translations[i]
	}
	return merged
}

// translationsFromResult extracts exactly count non-empty translations from
// an oneshot response body. oneshot answers `text` arrays positionally, so a
// batch is all-or-nothing: if upstream returns a different number of
// translations or leaves one of them empty, the caller fails as a whole
// instead of answering with a misaligned list or a placeholder.
func translationsFromResult(result gjson.Result, count int) ([]string, error) {
	translations := result.Get("translations").Array()
	if len(translations) != count {
		return nil, fmt.Errorf("expected %d translation(s), got %d", count, len(translations))
	}
	texts := make([]string, 0, count)
	for _, translation := range translations {
		text := translation.Get("text").String()
		if text == "" {
			return nil, errors.New("upstream returned an empty translation")
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// TranslateByDLX performs translation via the DeepL oneshot endpoint.
// texts holds one or more source strings: oneshot's `text` body field is an
// array, so a whole batch travels in a single upstream request and the
// returned Data stays position-aligned with texts. Passing dlSession switches
// to the Pro endpoint; the value is sent verbatim as the Bearer token (i.e.
// it must be an OAuth access token, not the legacy dl_session cookie).
func TranslateByDLX(sourceLang, targetLang string, texts []string, tagHandling string, proxyURL string, dlSession string) (DLXTranslationResult, error) {
	return TranslateByDLXContext(context.Background(), sourceLang, targetLang, texts, tagHandling, proxyURL, dlSession)
}

// TranslateByDLXContext cancels the upstream translation when ctx is done.
func TranslateByDLXContext(ctx context.Context, sourceLang, targetLang string, texts []string, tagHandling, proxyURL, dlSession string) (DLXTranslationResult, error) {
	if err := ctx.Err(); err != nil {
		return DLXTranslationResult{}, err
	}
	// oneshot charges its anonymous cap against the total length of the
	// `text` array, and a batch is one upstream request, so the cap applies
	// to the sum of the texts that are actually sent.
	positions, totalLength := textsToTranslate(texts)

	resolvedTarget, err := resolveTargetLang(targetLang)
	if err != nil {
		return DLXTranslationResult{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}, nil
	}
	resolvedSource, err := resolveSourceLang(sourceLang)
	if err != nil {
		return DLXTranslationResult{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}, nil
	}

	if totalLength > maxFreeTextLength {
		return DLXTranslationResult{
			Code:    http.StatusRequestEntityTooLarge,
			Message: fmt.Sprintf("text exceeds maximum length: %d characters (anonymous oneshot limit is %d)", totalLength, maxFreeTextLength),
		}, nil
	}

	// Nothing to translate: every text is blank, so none of them travels and
	// the request is answered with its own texts — an empty list in, an empty
	// list out — exactly as a blank element is inside a request that does have
	// something to translate.
	if len(positions) == 0 {
		return DLXTranslationResult{
			Code:         http.StatusOK,
			ID:           time.Now().UnixMilli(),
			Data:         mergeTranslations(texts, positions, nil),
			Alternatives: nil, // oneshot does not return alternatives
			SourceLang:   sourceLang,
			TargetLang:   targetLang,
			Method:       map[bool]string{true: "Pro", false: "Free"}[dlSession != ""],
		}, nil
	}

	// tagHandling is accepted by the public DLX API for compatibility
	// but oneshot does not expose html/xml tag handling the way the
	// official v2 API does — ignored upstream.
	_ = tagHandling

	// The free path presents a browser fingerprint and carries no iOS
	// app_information, so it never needs the App Store version. Only the Pro
	// path advertises one, in app_information.
	var appInfo *appInformation
	if dlSession != "" {
		appVersion := currentIOSAppVersion()
		if err := ctx.Err(); err != nil {
			return DLXTranslationResult{}, err
		}
		appInfo = &appInformation{
			OS:         "iOS",
			OSVersion:  iosOSVersion,
			AppVersion: appVersion,
			AppBuild:   iosAppBuild,
			InstanceID: instanceID,
		}
	}

	// Blank texts stay here: oneshot echoes an empty input, and the positional
	// check on the answer would read that echo as a failed translation and fail
	// the batch, so there is nothing to gain by sending them.
	requested := make([]string, 0, len(positions))
	for _, position := range positions {
		requested = append(requested, texts[position])
	}

	reqStruct := oneshotRequest{
		Text:       requested,
		TargetLang: resolvedTarget,
		SourceLang: resolvedSource, // empty = autodetect; omitempty drops the field
		// ItaClient.OneShotUsageType.translate (also: ocr, voiceforconversations)
		UsageType:      "translate",
		AppInformation: appInfo,
	}
	bodyBytes, _ := json.Marshal(reqStruct)

	endpoint := oneshotFreeEndpoint
	if dlSession != "" {
		endpoint = oneshotProEndpoint
	}

	id := time.Now().UnixMilli()
	response, err := callOneshot(ctx, endpoint, bodyBytes, dlSession, proxyURL)
	if err != nil {
		if ctx.Err() != nil {
			return DLXTranslationResult{}, ctx.Err()
		}
		// Map upstream timeouts to 504 so callers can distinguish "DeepL
		// took too long" from other 503 failure modes (DNS, TLS, etc.).
		var ue *url.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ue) && ue.Timeout()) {
			return DLXTranslationResult{
				ID:      id,
				Code:    http.StatusGatewayTimeout,
				Message: fmt.Sprintf("upstream DeepL request timed out after %s", oneshotTimeout),
			}, nil
		}
		return DLXTranslationResult{
			ID:      id,
			Code:    http.StatusServiceUnavailable,
			Message: "upstream DeepL request failed",
		}, nil
	}

	result := response.body
	switch response.status {
	case http.StatusOK:
		// fall through to body parsing
	case http.StatusTooManyRequests:
		return DLXTranslationResult{
			ID:         id,
			Code:       http.StatusTooManyRequests,
			Message:    "DeepL rate limited the request",
			RetryAfter: response.retryAfter,
		}, nil
	case http.StatusForbidden:
		// iOS surfaces this as OneShot: Forbidden / AuthenticationFailed /
		// OutdatedClient / UserBlocked depending on body; collapse to 403.
		msg := result.Get("title").String()
		if msg == "" {
			msg = result.Get("message").String()
		}
		if msg == "" {
			msg = "request forbidden by DeepL (auth failed, outdated client, or blocked)"
		}
		return DLXTranslationResult{
			ID:      id,
			Code:    http.StatusForbidden,
			Message: msg,
		}, nil
	default:
		return DLXTranslationResult{
			ID:      id,
			Code:    http.StatusServiceUnavailable,
			Message: fmt.Sprintf("request failed with status code: %d", response.status),
		}, nil
	}

	translations, err := translationsFromResult(result, len(requested))
	if err != nil {
		return DLXTranslationResult{
			ID:      id,
			Code:    http.StatusServiceUnavailable,
			Message: "Translation failed",
		}, nil
	}

	if detected := result.Get("translations.0.detected_source_language").String(); detected != "" {
		sourceLang = strings.ToUpper(detected)
	}

	return DLXTranslationResult{
		Code:         http.StatusOK,
		ID:           id,
		Data:         mergeTranslations(texts, positions, translations),
		Alternatives: nil, // oneshot does not return alternatives
		SourceLang:   sourceLang,
		TargetLang:   targetLang,
		Method:       map[bool]string{true: "Pro", false: "Free"}[dlSession != ""],
	}, nil
}
