package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"
)

type GeoInfo struct {
	CountryCode  string `json:"code"`
	CountryName  string `json:"country"`
	ISP          string `json:"isp"`
	IsDatacenter bool   `json:"is_dc"`
}

type GeoIPService struct {
	mu         sync.RWMutex
	cache      map[string]GeoInfo
	db         *geoip2.Reader
	httpClient *http.Client
}

var globalGeoIPService = newGeoIPService()

func newGeoIPService() *GeoIPService {
	s := &GeoIPService{
		cache: make(map[string]GeoInfo),
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
	s.initMMDB()
	return s
}

func (s *GeoIPService) initMMDB() {
	paths := []string{
		os.Getenv("GEOIP_DB_PATH"),
		"/data/GeoLite2-City.mmdb",
		"/home/customer/owncast/data/GeoLite2-City.mmdb",
		"/var/lib/GeoIP/GeoLite2-City.mmdb",
		"/usr/share/GeoIP/GeoLite2-City.mmdb",
		"./GeoLite2-City.mmdb",
	}

	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			db, err := geoip2.Open(p)
			if err == nil {
				s.db = db
				log.Printf("[GeoIP] Successfully loaded MaxMind database from %s", p)
				return
			}
		}
	}
	log.Printf("[GeoIP] No local MaxMind database found, using HTTP GeoIP fallback")
}

func sanitizeIP(ipStr string) string {
	ipStr = strings.TrimSpace(ipStr)
	if idx := strings.Index(ipStr, ":"); idx != -1 && !strings.Contains(ipStr, "]") && strings.Count(ipStr, ":") == 1 {
		ipStr = ipStr[:idx]
	}
	return ipStr
}

func (s *GeoIPService) Lookup(ipStr string) GeoInfo {
	ipStr = sanitizeIP(ipStr)
	if ipStr == "" || ipStr == "-" || ipStr == "127.0.0.1" || ipStr == "::1" || strings.HasPrefix(ipStr, "172.") || strings.HasPrefix(ipStr, "10.") || strings.HasPrefix(ipStr, "192.168.") {
		return GeoInfo{
			CountryCode:  "UG",
			CountryName:  "Uganda",
			ISP:          "Local Network",
			IsDatacenter: false,
		}
	}

	s.mu.RLock()
	info, found := s.cache[ipStr]
	s.mu.RUnlock()
	if found {
		return info
	}

	var cc, cname string
	if s.db != nil {
		parsedIP := net.ParseIP(ipStr)
		if parsedIP != nil {
			if record, err := s.db.City(parsedIP); err == nil {
				cc = record.Country.IsoCode
				cname = record.Country.Names["en"]
			}
		}
	}

	isp := ""
	isDC := false
	apiURL := fmt.Sprintf("http://ip-api.com/json/%s?fields=status,countryCode,country,isp,hosting", ipStr)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err == nil {
		resp, err := s.httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				var apiResp struct {
					Status      string `json:"status"`
					CountryCode string `json:"countryCode"`
					Country     string `json:"country"`
					ISP         string `json:"isp"`
					Hosting     bool   `json:"hosting"`
				}
				if err := json.Unmarshal(body, &apiResp); err == nil && apiResp.Status == "success" {
					if cc == "" {
						cc = apiResp.CountryCode
					}
					if cname == "" {
						cname = apiResp.Country
					}
					isp = apiResp.ISP
					isDC = apiResp.Hosting
				}
			}
		}
	}

	if cc == "" {
		cc = "UG"
	}
	if cname == "" {
		cname = "Uganda"
	}
	if isp == "" {
		isp = "Cellular/Broadband"
	}

	dcKeywords := []string{"hetzner", "digitalocean", "vultr", "linode", "contabo", "ovh", "scaleway", "upcloud", "amazon", "amazonaws", "azure", "google", "cloudflare", "fastly", "akamai", "hosting", "datacenter", "vps"}
	ispLower := strings.ToLower(isp)
	for _, kw := range dcKeywords {
		if strings.Contains(ispLower, kw) {
			isDC = true
			break
		}
	}

	info = GeoInfo{
		CountryCode:  cc,
		CountryName:  cname,
		ISP:          isp,
		IsDatacenter: isDC,
	}

	s.mu.Lock()
	s.cache[ipStr] = info
	s.mu.Unlock()

	return info
}
