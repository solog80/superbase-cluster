package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
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
	s.loadTSV()
	s.initMMDB()
	return s
}

func (s *GeoIPService) loadTSV() {
	paths := []string{
		os.Getenv("GEO_TSV_PATH"),
		"/cache/geo.tsv",
		"/home/customer/vstats/cache/geo.tsv",
		"/home/customer/shipper/state/geo.tsv",
		"/data/geo.tsv",
		"./geo.tsv",
	}

	for _, p := range paths {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		count := 0
		s.mu.Lock()
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) >= 4 {
				ip := strings.TrimSpace(parts[0])
				cc := strings.TrimSpace(parts[1])
				cname := strings.TrimSpace(parts[2])
				isp := strings.TrimSpace(parts[3])
				if ip != "" && isp != "" {
					s.cache[ip] = GeoInfo{
						CountryCode: cc,
						CountryName: cname,
						ISP:         isp,
					}
					count++
				}
			}
		}
		s.mu.Unlock()
		_ = f.Close()
		if count > 0 {
			log.Printf("[GeoIP] Loaded %d IP mappings from TSV: %s", count, p)
			break
		}
	}
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
		isp = "Broadband Provider"
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

// GetCountryISPBreakdown dynamically aggregates real ISP frequencies for a given country
// from the loaded 37k+ GeoIP TSV cache and scales them to totalViewers.
func (s *GeoIPService) GetCountryISPBreakdown(cName string, totalViewers int) []ispItem {
	code := getIsoCode(cName)
	cNameLower := strings.ToLower(strings.TrimSpace(cName))

	s.mu.RLock()
	counts := make(map[string]int)
	totalKnownIPs := 0
	for _, info := range s.cache {
		if strings.ToLower(strings.TrimSpace(info.CountryName)) == cNameLower || strings.ToLower(strings.TrimSpace(info.CountryCode)) == cNameLower {
			isp := strings.TrimSpace(info.ISP)
			if isp != "" && isp != "Cellular/Broadband" && isp != "Local Network" {
				counts[isp]++
				totalKnownIPs++
			}
		}
	}
	s.mu.RUnlock()

	if totalKnownIPs == 0 || len(counts) == 0 {
		return []ispItem{{Code: code, ISP: "Broadband Provider", Viewers: totalViewers}}
	}

	type kv struct {
		isp   string
		count int
	}
	var sorted []kv
	for k, v := range counts {
		sorted = append(sorted, kv{isp: k, count: v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	var items []ispItem
	allocated := 0
	limit := 5
	if len(sorted) < limit {
		limit = len(sorted)
	}
	for i := 0; i < limit; i++ {
		v := int(float64(sorted[i].count) / float64(totalKnownIPs) * float64(totalViewers))
		if v < 1 {
			v = 1
		}
		allocated += v
		items = append(items, ispItem{
			Code:    code,
			ISP:     sorted[i].isp,
			Viewers: v,
		})
	}
	if len(items) > 0 && allocated < totalViewers {
		items[0].Viewers += (totalViewers - allocated)
	}
	return items
}
