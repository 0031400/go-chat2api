package main

import (
	"bytes"
	"context"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	mrand "math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"
)

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
		return errNoDPLBuildFound
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
