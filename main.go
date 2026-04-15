package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	crand "crypto/rand"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	ListenAddr string
	BaseURL    string
	Timeout    time.Duration
	PowDifficulty string
	OAILanguage string
}

type Server struct {
	cfg    Config
	client *http.Client
	dplMu sync.Mutex
	cachedDPLScript string
	cachedDPLBuild string
	cachedDPLTime time.Time
}

type ChatCompletionRequest struct {
	Model     string         `json:"model"`
	Messages  []InputMessage `json:"messages"`
	Stream    bool           `json:"stream"`
	MaxTokens int            `json:"max_tokens,omitempty"`
}

type InputMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type RequirementsResponse struct {
	Token   string `json:"token"`
	Persona string `json:"persona"`
	Turnstile struct {
		Required bool `json:"required"`
		Dx string `json:"dx"`
	} `json:"turnstile"`
	Arkose struct {
		Required bool `json:"required"`
		Dx string `json:"dx"`
	} `json:"arkose"`
	ProofOfWork struct {
		Required bool `json:"required"`
		Seed string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

type ChatGPTConversationRequest struct {
	Action                     string                   `json:"action"`
	ConversationMode           map[string]interface{}   `json:"conversation_mode"`
	ForceParagen               bool                     `json:"force_paragen"`
	ForceParagenModelSlug      string                   `json:"force_paragen_model_slug"`
	ForceRateLimit             bool                     `json:"force_rate_limit"`
	ForceUseSSE                bool                     `json:"force_use_sse"`
	HistoryAndTrainingDisabled bool                     `json:"history_and_training_disabled"`
	Messages                   []map[string]interface{} `json:"messages"`
	Model                      string                   `json:"model"`
	ParentMessageID            string                   `json:"parent_message_id"`
	ResetRateLimits            bool                     `json:"reset_rate_limits"`
	Suggestions                []interface{}            `json:"suggestions"`
	SupportedEncodings         []interface{}            `json:"supported_encodings"`
	SystemHints                []interface{}            `json:"system_hints"`
	Timezone                   string                   `json:"timezone"`
	TimezoneOffsetMin          int                      `json:"timezone_offset_min"`
	VariantPurpose             string                   `json:"variant_purpose"`
	WebsocketRequestID         string                   `json:"websocket_request_id"`
	ClientContextualInfo       map[string]interface{}   `json:"client_contextual_info"`
}

var modelProxy = map[string]string{
	"gpt-3.5-turbo":        "gpt-3.5-turbo-0125",
	"gpt-3.5-turbo-16k":    "gpt-3.5-turbo-16k-0613",
	"gpt-4":                "gpt-4-0613",
	"gpt-4-32k":            "gpt-4-32k-0613",
	"gpt-4-turbo-preview":  "gpt-4-0125-preview",
	"gpt-4-vision-preview": "gpt-4-1106-vision-preview",
	"gpt-4-turbo":          "gpt-4-turbo-2024-04-09",
	"gpt-4o":               "gpt-4o-2024-08-06",
	"gpt-4o-mini":          "gpt-4o-mini-2024-07-18",
	"o1-preview":           "o1-preview-2024-09-12",
	"o1-mini":              "o1-mini-2024-09-12",
	"o1":                   "o1-2024-12-18",
	"o3-mini":              "o3-mini-2025-01-31",
	"o3-mini-high":         "o3-mini-high-2025-01-31",
}

func main() {
	mrand.Seed(time.Now().UnixNano())
	cfg := Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		BaseURL:    strings.TrimRight(getEnv("CHATGPT_BASE_URL", "https://chatgpt.com"), "/"),
		Timeout:    90 * time.Second,
		PowDifficulty: getEnv("POW_DIFFICULTY", "000032"),
		OAILanguage: getEnv("OAI_LANGUAGE", "zh-CN"),
	}
	jar, _ := cookiejar.New(nil)

	srv := &Server{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Jar: jar,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleRoot)
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/chat/completions", srv.handleChatCompletions)

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, loggingMiddleware(mux)); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "go-chat2api",
		"status":  "ok",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := compactID()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	accessToken, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json_body")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages_required")
		return
	}

	ctx := r.Context()
	deviceID := newUUID()
	userAgent := defaultUserAgent()
	log.Printf("[%s] incoming model=%q stream=%v messages=%d", reqID, req.Model, req.Stream, len(req.Messages))
	respModel := modelProxy[req.Model]
	if respModel == "" {
		respModel = req.Model
		if respModel == "" {
			respModel = "gpt-3.5-turbo-0125"
		}
	}
	reqModel := mapRequestModel(req.Model)
	log.Printf("[%s] model mapping: origin=%q req_model=%q resp_model=%q", reqID, req.Model, reqModel, respModel)
	log.Printf("[%s] fp user_agent=%q oai_device_id=%q", reqID, userAgent, deviceID)

	chatMessages, err := s.convertMessages(ctx, req.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	payload := ChatGPTConversationRequest{
		Action:                     "next",
		ConversationMode:           map[string]interface{}{"kind": "primary_assistant"},
		ForceParagen:               false,
		ForceParagenModelSlug:      "",
		ForceRateLimit:             false,
		ForceUseSSE:                true,
		HistoryAndTrainingDisabled: false,
		Messages:                   chatMessages,
		Model:                      reqModel,
		ParentMessageID:            newUUID(),
		ResetRateLimits:            false,
		Suggestions:                []interface{}{},
		SupportedEncodings:         []interface{}{},
		SystemHints:                []interface{}{},
		Timezone:                   "America/Los_Angeles",
		TimezoneOffsetMin:          -480,
		VariantPurpose:             "comparison_implicit",
		WebsocketRequestID:         newUUID(),
		ClientContextualInfo: map[string]interface{}{
			"is_dark_mode":      false,
			"time_since_loaded": 100,
			"page_height":       900,
			"page_width":        1440,
			"pixel_ratio":       1.5,
			"screen_height":     1080,
			"screen_width":      1920,
		},
	}

	if err := s.ensureDPL(ctx, reqID, accessToken, deviceID, userAgent); err != nil {
		log.Printf("[%s] ensure dpl failed: %v", reqID, err)
	}

	reqResp, requirementsToken, err := s.fetchChatRequirements(ctx, reqID, accessToken, deviceID, userAgent)
	if err != nil {
		log.Printf("[%s] chat requirements unavailable: %v", reqID, err)
	}

	proofToken := ""
	if reqResp != nil && reqResp.ProofOfWork.Required {
		if strings.Compare(reqResp.ProofOfWork.Difficulty, s.cfg.PowDifficulty) <= 0 {
			log.Printf("[%s] proof difficulty too high diff=%q threshold=%q", reqID, reqResp.ProofOfWork.Difficulty, s.cfg.PowDifficulty)
		} else {
			config := s.buildPowConfig(userAgent)
			tk, solved := getAnswerToken(reqResp.ProofOfWork.Seed, reqResp.ProofOfWork.Difficulty, config)
			log.Printf("[%s] proof solved=%v token_present=%v", reqID, solved, tk != "")
			if solved {
				proofToken = tk
			}
		}
	}
	if reqResp != nil && reqResp.Turnstile.Required {
		log.Printf("[%s] turnstile required but solver not implemented in go version", reqID)
	}

	if err := s.proxyConversation(ctx, reqID, accessToken, requirementsToken, proofToken, payload, respModel, req.Stream, deviceID, userAgent, w); err != nil {
		log.Printf("[%s] proxy conversation failed: %v", reqID, err)
	}
}

func (s *Server) proxyConversation(ctx context.Context, reqID, accessToken, requirementsToken, proofToken string, payload ChatGPTConversationRequest, respModel string, stream bool, deviceID, userAgent string, w http.ResponseWriter) error {
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_request_failed")
		return err
	}
	log.Printf("[%s] conversation request bytes=%d requirements_token_present=%v", reqID, len(body), requirementsToken != "")
	log.Printf("[%s] conversation request model=%q stream=%v", reqID, payload.Model, stream)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/backend-api/conversation", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build_request_failed")
		return err
	}
	setBaseHeaders(req.Header, accessToken, deviceID, userAgent, s.cfg.OAILanguage)
	req.Header.Set("Accept", "text/event-stream")
	if requirementsToken != "" {
		req.Header.Set("openai-sentinel-chat-requirements-token", requirementsToken)
	}
	if proofToken != "" {
		req.Header.Set("openai-sentinel-proof-token", proofToken)
	}
	log.Printf("[%s] conversation request headers: %s", reqID, headerSnapshot(req.Header))

	resp, err := s.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "chatgpt_request_failed")
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		blob, readErr := readPossiblyCompressedBody(resp)
		log.Printf("[%s] conversation status=%d ct=%q ce=%q read_err=%v", reqID, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), readErr)
		log.Printf("[%s] conversation response headers: %s", reqID, headerSnapshot(resp.Header))
		log.Printf("[%s] conversation body preview: %s", reqID, previewBytes(blob))
		contentType := resp.Header.Get("Content-Type")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(blob)
		return fmt.Errorf("chatgpt returned status %d", resp.StatusCode)
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_, err = io.Copy(w, resp.Body)
		return err
	}

	content, err := readAssistantText(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "parse_sse_failed")
		return err
	}

	completion := map[string]interface{}{
		"id":      "chatcmpl-" + compactID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   respModel,
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	writeJSON(w, http.StatusOK, completion)
	return nil
}

func (s *Server) fetchChatRequirements(ctx context.Context, reqID, accessToken, deviceID, userAgent string) (*RequirementsResponse, string, error) {
	pToken := s.buildRequirementsToken(userAgent)
	payloadMap := map[string]string{"p": pToken}
	payload, _ := json.Marshal(payloadMap)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/backend-api/sentinel/chat-requirements", bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	setBaseHeaders(req.Header, accessToken, deviceID, userAgent, s.cfg.OAILanguage)
	req.Header.Set("Accept", "*/*")
	log.Printf("[%s] chat-requirements request headers: %s", reqID, headerSnapshot(req.Header))
	log.Printf("[%s] chat-requirements request p_token_prefix=%q", reqID, clipToken(pToken, 16))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, readErr := readPossiblyCompressedBody(resp)
	log.Printf("[%s] chat-requirements status=%d ct=%q ce=%q read_err=%v", reqID, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), readErr)
	log.Printf("[%s] chat-requirements response headers: %s", reqID, headerSnapshot(resp.Header))
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d: %s", resp.StatusCode, previewBytes(data))
	}
	var out RequirementsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		log.Printf("[%s] chat-requirements json parse failed: %v", reqID, err)
		log.Printf("[%s] chat-requirements raw preview: %s", reqID, previewBytes(data))
		return nil, "", err
	}
	log.Printf("[%s] chat-requirements persona=%q token_present=%v turnstile=%v arkose=%v pow=%v",
		reqID, out.Persona, out.Token != "", out.Turnstile.Required, out.Arkose.Required, out.ProofOfWork.Required)
	log.Printf("[%s] chat-requirements pow_seed_prefix=%q pow_diff=%q", reqID, clipToken(out.ProofOfWork.Seed, 20), out.ProofOfWork.Difficulty)
	if out.Token == "" {
		return &out, "", errors.New("empty requirements token")
	}
	return &out, out.Token, nil
}

func (s *Server) convertMessages(_ context.Context, input []InputMessage) ([]map[string]interface{}, error) {
	out := make([]map[string]interface{}, 0, len(input))
	for _, msg := range input {
		chatMsg, err := convertSingleMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, chatMsg)
	}
	return out, nil
}

func convertSingleMessage(msg InputMessage) (map[string]interface{}, error) {
	switch content := msg.Content.(type) {
	case string:
		return map[string]interface{}{
			"id":     newUUID(),
			"author": map[string]interface{}{"role": msg.Role},
			"content": map[string]interface{}{
				"content_type": "text",
				"parts":        []interface{}{content},
			},
			"metadata": map[string]interface{}{},
		}, nil
	case []interface{}:
		parts := make([]interface{}, 0, len(content))
		for _, item := range content {
			obj, ok := item.(map[string]interface{})
			if !ok {
				return nil, errors.New("invalid_message_content_item")
			}
			kind, _ := obj["type"].(string)
			switch kind {
			case "text":
				text, _ := obj["text"].(string)
				parts = append(parts, text)
			case "image_url":
				return nil, errors.New("image_url_not_supported_yet")
			default:
				return nil, fmt.Errorf("unsupported_content_type_%s", kind)
			}
		}
		return map[string]interface{}{
			"id":     newUUID(),
			"author": map[string]interface{}{"role": msg.Role},
			"content": map[string]interface{}{
				"content_type": "multimodal_text",
				"parts":        parts,
			},
			"metadata": map[string]interface{}{},
		}, nil
	default:
		return nil, errors.New("unsupported_message_content")
	}
}

func readAssistantText(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	events := strings.Split(string(data), "\n\n")
	for i := len(events) - 1; i >= 0; i-- {
		line := strings.TrimSpace(events[i])
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		msg, _ := evt["message"].(map[string]interface{})
		content, _ := msg["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		if len(parts) == 0 {
			continue
		}
		if text, ok := parts[0].(string); ok && text != "" {
			return text, nil
		}
	}
	return "", errors.New("assistant_text_not_found")
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
		// Unknown encoding (e.g. br/zstd): return raw bytes for diagnostics.
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
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

func (s *Server) ensureDPL(ctx context.Context, reqID, accessToken, deviceID, userAgent string) error {
	s.dplMu.Lock()
	if !s.cachedDPLTime.IsZero() && time.Since(s.cachedDPLTime) < 15*time.Minute && s.cachedDPLBuild != "" {
		s.dplMu.Unlock()
		return nil
	}
	s.dplMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/", nil)
	if err != nil {
		return err
	}
	setBaseHeaders(req.Header, accessToken, deviceID, userAgent, s.cfg.OAILanguage)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, readErr := readPossiblyCompressedBody(resp)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, previewBytes(body))
	}
	html := string(body)
	script, build := parseDPLFromHTML(html)
	if build == "" {
		return errors.New("no dpl build found")
	}
	s.dplMu.Lock()
	s.cachedDPLScript = script
	s.cachedDPLBuild = build
	s.cachedDPLTime = time.Now()
	s.dplMu.Unlock()
	log.Printf("[%s] dpl updated build=%q script=%q", reqID, build, script)
	return nil
}

func parseDPLFromHTML(html string) (string, string) {
	build := ""
	script := "https://chatgpt.com/backend-api/sentinel/sdk.js"
	reBuild := regexp.MustCompile(`data-build="([^"]+)"`)
	mBuild := reBuild.FindStringSubmatch(html)
	if len(mBuild) >= 2 {
		build = mBuild[1]
	}
	reScript := regexp.MustCompile(`src="([^"]+)"`)
	matches := reScript.FindAllStringSubmatch(html, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		src := m[1]
		if strings.Contains(src, "c/") && strings.Contains(src, "/_") {
			script = src
			reDpl := regexp.MustCompile(`c/[^/]*/_`)
			md := reDpl.FindStringSubmatch(src)
			if len(md) > 0 {
				build = md[0]
			}
			break
		}
	}
	return script, build
}

func (s *Server) buildPowConfig(userAgent string) []interface{} {
	s.dplMu.Lock()
	script := s.cachedDPLScript
	build := s.cachedDPLBuild
	s.dplMu.Unlock()
	if script == "" {
		script = "https://chatgpt.com/backend-api/sentinel/sdk.js"
	}
	parseTime := time.Now().In(time.FixedZone("EST", -5*3600)).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
	cores := []int{8, 16, 24, 32}
	navigatorKeys := []string{
		"vendor↦Google Inc.",
		"webdriver↦false",
		"cookieEnabled↦true",
	}
	documentKeys := []string{"location", "_reactListeningo743lnnpvdg"}
	windowKeys := []string{"window", "document", "navigator", "location", "history"}
	return []interface{}{
		chooseInt([]int{3000, 4000, 3120, 4160}),
		parseTime,
		4294705152,
		0,
		userAgent,
		script,
		build,
		"en-US",
		"en-US,es-US,en,es",
		0,
		chooseStr(navigatorKeys),
		chooseStr(documentKeys),
		chooseStr(windowKeys),
		float64(time.Now().UnixNano()) / 1e6,
		newUUID(),
		"",
		chooseInt(cores),
		float64(time.Now().UnixMilli()) - (mrand.Float64() * 1000),
	}
}

func (s *Server) buildRequirementsToken(userAgent string) string {
	config := s.buildPowConfig(userAgent)
	seed := fmt.Sprintf("%f", mrand.Float64())
	answer, _ := generateAnswer(seed, "0fffff", config)
	return "gAAAAAC" + answer
}

func getAnswerToken(seed, diff string, config []interface{}) (string, bool) {
	answer, solved := generateAnswer(seed, diff, config)
	return "gAAAAAB" + answer, solved
}

func generateAnswer(seed, diff string, config []interface{}) (string, bool) {
	targetDiff, err := hex.DecodeString(diff)
	if err != nil {
		return "", false
	}
	seedBytes := []byte(seed)
	diffLen := len(diff)
	if diffLen <= 0 {
		return "", false
	}
	maxIter := 500000
	for i := 0; i < maxIter; i++ {
		payload := make([]interface{}, len(config))
		copy(payload, config)
		payload[3] = i
		payload[9] = i >> 1
		b, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		enc := base64.StdEncoding.EncodeToString(b)
		input := make([]byte, 0, len(seedBytes)+len(enc))
		input = append(input, seedBytes...)
		input = append(input, enc...)
		h := sha3.Sum512(input)
		candidate := h[:]
		if diffLen < len(candidate) {
			candidate = candidate[:diffLen]
		}
		if bytes.Compare(candidate, targetDiff) <= 0 {
			return enc, true
		}
	}
	fallback := "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString([]byte(`"`+seed+`"`))
	return fallback, false
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
