package main

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// loadDotEnv reads a dotenv-style file (KEY="value" / KEY=value lines) into the
// process environment WITHOUT overriding variables already set (e.g. injected
// via docker run -e on the mesh). Search order: ENV_FILE, then ./.env, then
// ../.env (functions/.env when running from functions/superbase).
func loadDotEnv() {
	paths := []string{os.Getenv("ENV_FILE")}
	for _, p := range []string{".env", "../.env"} {
		if p != "" {
			paths = append(paths, p)
		}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		count := 0
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			eq := strings.Index(line, "=")
			if eq < 0 {
				continue
			}
			k := strings.TrimSpace(line[:eq])
			v := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
			if k == "" {
				continue
			}
			if _, exists := os.LookupEnv(k); exists {
				continue
			}
			os.Setenv(k, v)
			count++
		}
		if count > 0 {
			log.Printf("loaded %d vars from %s", count, path)
			return
		}
	}
}
