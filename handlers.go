package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

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
		ConversationMode:           map[string]any{"kind": "primary_assistant"},
		ForceParagen:               false,
		ForceParagenModelSlug:      "",
		ForceRateLimit:             false,
		ForceUseSSE:                true,
		HistoryAndTrainingDisabled: false,
		Messages:                   chatMessages,
		Model:                      reqModel,
		ParentMessageID:            newUUID(),
		ResetRateLimits:            false,
		Suggestions:                []any{},
		SupportedEncodings:         []any{},
		SystemHints:                []any{},
		Timezone:                   "America/Los_Angeles",
		TimezoneOffsetMin:          -480,
		VariantPurpose:             "comparison_implicit",
		WebsocketRequestID:         newUUID(),
		ClientContextualInfo: map[string]any{
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

func (s *Server) convertMessages(_ context.Context, input []InputMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(input))
	for _, msg := range input {
		chatMsg, err := convertSingleMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, chatMsg)
	}
	return out, nil
}

func convertSingleMessage(msg InputMessage) (map[string]any, error) {
	switch content := msg.Content.(type) {
	case string:
		return map[string]any{
			"id":     newUUID(),
			"author": map[string]any{"role": msg.Role},
			"content": map[string]any{
				"content_type": "text",
				"parts":        []any{content},
			},
			"metadata": map[string]any{},
		}, nil
	case []any:
		parts := make([]any, 0, len(content))
		for _, item := range content {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, errInvalidMessageContentItem
			}
			kind, _ := obj["type"].(string)
			switch kind {
			case "text":
				text, _ := obj["text"].(string)
				parts = append(parts, text)
			case "image_url":
				return nil, errImageURLNotSupported
			default:
				return nil, fmt.Errorf("unsupported_content_type_%s", kind)
			}
		}
		return map[string]any{
			"id":     newUUID(),
			"author": map[string]any{"role": msg.Role},
			"content": map[string]any{
				"content_type": "multimodal_text",
				"parts":        parts,
			},
			"metadata": map[string]any{},
		}, nil
	default:
		return nil, errUnsupportedMessageContent
	}
}

