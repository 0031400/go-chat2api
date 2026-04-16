package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/cookiejar"
)

func main() {
	configPath := flag.String("c", "config.json", "path to config json file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load %s failed: %v", *configPath, err)
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
	mux.HandleFunc("/v1/chat/completions", srv.handleChatCompletions)

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, loggingMiddleware(mux)); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}
