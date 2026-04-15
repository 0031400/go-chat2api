package main

import (
	"compress/gzip"
	"compress/zlib"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errInvalidMessageContentItem = errors.New("invalid_message_content_item")
	errImageURLNotSupported      = errors.New("image_url_not_supported_yet")
	errUnsupportedMessageContent = errors.New("unsupported_message_content")
	errAssistantTextNotFound     = errors.New("assistant_text_not_found")
	errEmptyRequirementsToken    = errors.New("empty requirements token")
	errNoDPLBuildFound           = errors.New("no dpl build found")
)

func unsupportedContentType(kind string) error {
	return fmt.Errorf("unsupported_content_type_%s", kind)
}

func mapRequestModel(origin string) string {
	switch {
	case strings.Contains(origin, "o3-mini-high"):
		return "o3-mini-high"
	case strings.Contains(origin, "o3-mini-medium"):
		return "o3-mini-medium"
	case strings.Contains(origin, "o3-mini-low"):
		return "o3-mini-low"
	case strings.Contains(origin, "o3-mini"):
		return "o3-mini"
	case strings.Contains(origin, "o3"):
		return "o3"
	case strings.Contains(origin, "o1-preview"):
		return "o1-preview"
	case strings.Contains(origin, "o1-pro"):
		return "o1-pro"
	case strings.Contains(origin, "o1-mini"):
		return "o1-mini"
	case strings.Contains(origin, "o1"):
		return "o1"
	case strings.Contains(origin, "gpt-4.5o"):
		return "gpt-4.5o"
	case strings.Contains(origin, "gpt-4o-canmore"):
		return "gpt-4o-canmore"
	case strings.Contains(origin, "gpt-4o-mini"):
		return "gpt-4o-mini"
	case strings.Contains(origin, "gpt-4o"):
		return "gpt-4o"
	case strings.Contains(origin, "gpt-4-mobile"):
		return "gpt-4-mobile"
	case strings.Contains(origin, "gpt-4"):
		return "gpt-4"
	case strings.Contains(origin, "gpt-3.5"):
		return "text-davinci-002-render-sha"
	case strings.Contains(origin, "auto"):
		return "auto"
	default:
		return "gpt-4o"
	}
}

func setBaseHeaders(h http.Header, accessToken, deviceID, userAgent, oaiLanguage string) {
	h.Set("Accept", "*/*")
	h.Set("Accept-Encoding", "gzip, deflate")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", "https://chatgpt.com")
	h.Set("Priority", "u=1, i")
	h.Set("Referer", "https://chatgpt.com/")
	h.Set("OAI-Language", oaiLanguage)
	h.Set("User-Agent", userAgent)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("OAI-Device-Id", deviceID)
}

func decodeResponseStream(resp *http.Response) (io.ReadCloser, error) {
	ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch ce {
	case "", "identity":
		return resp.Body, nil
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return gr, nil
	case "deflate":
		zr, err := zlib.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return zr, nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", ce)
	}
}

func readPossiblyCompressedBody(resp *http.Response) ([]byte, error) {
	reader := io.Reader(resp.Body)
	ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch ce {
	case "", "identity":
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = gr
	case "deflate":
		zr, err := zlib.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	default:
		return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	}
	return io.ReadAll(io.LimitReader(reader, 2<<20))
}

func previewBytes(b []byte) string {
	if len(b) == 0 {
		return "<empty>"
	}
	if utf8.Valid(b) {
		s := string(b)
		if len(s) > 800 {
			s = s[:800] + "...(truncated)"
		}
		return s
	}
	n := len(b)
	if n > 128 {
		n = 128
	}
	return "non-utf8 bytes hex=" + hex.EncodeToString(b[:n])
}

func headerSnapshot(h http.Header) string {
	keys := []string{
		"Content-Type",
		"Content-Encoding",
		"Cf-Mitigated",
		"Set-Cookie",
		"Openai-Sentinel-Chat-Requirements-Token",
		"Openai-Sentinel-Proof-Token",
		"Openai-Sentinel-Arkose-Token",
		"Openai-Sentinel-Turnstile-Token",
		"Oai-Device-Id",
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.TrimSpace(h.Get(k))
		if v == "" {
			continue
		}
		if strings.Contains(strings.ToLower(k), "token") && len(v) > 20 {
			v = v[:20] + "...(masked)"
		}
		parts = append(parts, k+"="+v)
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errors.New("missing_authorization_header")
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("invalid_bearer_token")
	}
	return strings.TrimSpace(parts[1]), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func compactID() string {
	id := strings.ReplaceAll(newUUID(), "-", "")
	if len(id) > 24 {
		return id[:24]
	}
	return id
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func clipToken(v string, n int) string {
	if v == "" {
		return ""
	}
	if len(v) <= n {
		return v
	}
	return v[:n] + "...(masked)"
}

func chooseInt(list []int) int {
	if len(list) == 0 {
		return 0
	}
	return list[mrand.Intn(len(list))]
}

func chooseStr(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[mrand.Intn(len(list))]
}

func defaultUserAgent() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0"
}

func nowUnix() int64 {
	return time.Now().Unix()
}

