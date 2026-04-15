package main

import (
	"log"
	mrand "math/rand"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

func main() {
	mrand.Seed(time.Now().UnixNano())
	cfg := Config{
		ListenAddr:    getEnv("LISTEN_ADDR", ":8080"),
		BaseURL:       strings.TrimRight(getEnv("CHATGPT_BASE_URL", "https://chatgpt.com"), "/"),
		Timeout:       90 * time.Second,
		PowDifficulty: getEnv("POW_DIFFICULTY", "000032"),
		OAILanguage:   getEnv("OAI_LANGUAGE", "zh-CN"),
	}
	jar, _ := cookiejar.New(nil)

	srv := &Server{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Jar:     jar,
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
