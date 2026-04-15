package main

import (
	"net/http"
	"sync"
	"time"
)

type Config struct {
	ListenAddr    string
	BaseURL       string
	Timeout       time.Duration
	PowDifficulty string
	OAILanguage   string
}

type Server struct {
	cfg    Config
	client *http.Client

	dplMu           sync.Mutex
	cachedDPLScript string
	cachedDPLBuild  string
	cachedDPLTime   time.Time
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
		Required bool   `json:"required"`
		Dx       string `json:"dx"`
	} `json:"turnstile"`
	Arkose struct {
		Required bool   `json:"required"`
		Dx       string `json:"dx"`
	} `json:"arkose"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
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

const moderationMessage = "I'm sorry, I cannot provide or engage in any content related to pornography, violence, or any unethical material. If you have any other questions or need assistance, please feel free to let me know. I'll do my best to provide support and assistance."
