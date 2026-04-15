package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

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
		decoded, derr := decodeResponseStream(resp)
		if derr != nil {
			writeError(w, http.StatusBadGateway, "decode_stream_failed")
			return derr
		}
		defer decoded.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		return streamAsOpenAIChunks(decoded, w, respModel)
	}

	decoded, derr := decodeResponseStream(resp)
	if derr != nil {
		writeError(w, http.StatusBadGateway, "decode_stream_failed")
		return derr
	}
	defer decoded.Close()
	content, err := readAssistantText(decoded)
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
		return &out, "", errEmptyRequirementsToken
	}
	return &out, out.Token, nil
}
