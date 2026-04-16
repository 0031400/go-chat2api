package main

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

type jsonConfig struct {
	ListenAddr    string `json:"listen_addr"`
	BaseURL       string `json:"base_url"`
	TimeoutSecs   int    `json:"timeout_secs"`
	PowDifficulty string `json:"pow_difficulty"`
	OAILanguage   string `json:"oai_language"`
}

func defaultConfig() Config {
	return Config{
		ListenAddr:    ":8080",
		BaseURL:       "https://chatgpt.com",
		Timeout:       90 * time.Second,
		PowDifficulty: "000032",
		OAILanguage:   "zh-CN",
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	var jc jsonConfig
	if err := json.Unmarshal(data, &jc); err != nil {
		return cfg, err
	}

	if v := strings.TrimSpace(jc.ListenAddr); v != "" {
		cfg.ListenAddr = v
	}
	if v := strings.TrimSpace(jc.BaseURL); v != "" {
		cfg.BaseURL = strings.TrimRight(v, "/")
	}
	if jc.TimeoutSecs > 0 {
		cfg.Timeout = time.Duration(jc.TimeoutSecs) * time.Second
	}
	if v := strings.TrimSpace(jc.PowDifficulty); v != "" {
		cfg.PowDifficulty = v
	}
	if v := strings.TrimSpace(jc.OAILanguage); v != "" {
		cfg.OAILanguage = v
	}

	return cfg, nil
}
