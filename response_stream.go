package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

func readAssistantText(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lastText := ""
	activeMessageID := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		msg, _ := evt["message"].(map[string]any)
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]any)
		role, _ := author["role"].(string)
		if role == "user" || role == "system" {
			continue
		}
		messageID, _ := msg["id"].(string)
		status, _ := msg["status"].(string)
		endTurn, _ := msg["end_turn"].(bool)
		if activeMessageID == "" && status == "in_progress" && messageID != "" {
			activeMessageID = messageID
		}
		if activeMessageID == "" && status == "finished_successfully" && endTurn && messageID != "" {
			activeMessageID = messageID
		}
		if activeMessageID != "" && messageID != "" && messageID != activeMessageID {
			continue
		}
		content, _ := msg["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 0 {
			continue
		}
		if text, ok := parts[0].(string); ok && text != "" {
			lastText = text
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if lastText == "" {
		return "", errAssistantTextNotFound
	}
	return lastText, nil
}

func streamAsOpenAIChunks(r io.Reader, w io.Writer, model string) error {
	chunkID := "chatcmpl-" + compactID()
	created := nowUnix()
	firstChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"content": "",
			},
			"logprobs":      nil,
			"finish_reason": nil,
		}},
	}
	if err := writeSSEEvent(w, firstChunk); err != nil {
		return err
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lastText := ""
	activeMessageID := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}

		if t, _ := evt["type"].(string); t == "moderation" {
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"content": moderationMessage,
					},
					"logprobs":      nil,
					"finish_reason": "stop",
				}},
			}
			if err := writeSSEEvent(w, chunk); err != nil {
				return err
			}
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		}

		msg, _ := evt["message"].(map[string]any)
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]any)
		role, _ := author["role"].(string)
		if role == "user" || role == "system" {
			continue
		}
		messageID, _ := msg["id"].(string)
		status, _ := msg["status"].(string)
		endTurn, _ := msg["end_turn"].(bool)
		if activeMessageID == "" && status == "in_progress" && messageID != "" {
			activeMessageID = messageID
		}
		if activeMessageID == "" && status == "finished_successfully" && endTurn && messageID != "" {
			activeMessageID = messageID
		}
		if activeMessageID != "" && messageID != "" && messageID != activeMessageID {
			continue
		}
		content, _ := msg["content"].(map[string]any)
		outerType, _ := content["content_type"].(string)
		if outerType != "text" {
			continue
		}
		parts, _ := content["parts"].([]any)
		if len(parts) == 0 {
			continue
		}
		text, _ := parts[0].(string)
		if text == "" && status == "in_progress" {
			continue
		}

		newText := ""
		if strings.HasPrefix(text, lastText) {
			newText = text[len(lastText):]
		} else {
			newText = text
		}
		if newText != "" {
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"content": newText,
					},
					"logprobs":      nil,
					"finish_reason": nil,
				}},
			}
			if err := writeSSEEvent(w, chunk); err != nil {
				return err
			}
		}
		if text != "" {
			lastText = text
		}

		if status == "finished_successfully" {
			if endTurn {
				finalChunk := map[string]any{
					"id":      chunkID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index":         0,
						"delta":         map[string]any{},
						"logprobs":      nil,
						"finish_reason": "stop",
					}},
				}
				if err := writeSSEEvent(w, finalChunk); err != nil {
					return err
				}
				_, err := io.WriteString(w, "data: [DONE]\n\n")
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func writeSSEEvent(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "data: "+string(b)+"\n\n")
	return err
}

