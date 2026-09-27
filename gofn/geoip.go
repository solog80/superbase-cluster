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

func normalizeISPName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "-" {
		return "Broadband Provider"
	}

	lower := strings.ToLower(raw)

	if strings.Contains(lower, "airtel") || strings.Contains(lower, "mobile data service") {
		return "Airtel"
	}
	if strings.Contains(lower, "mtn") {
		return "MTN"
	}
	if strings.Contains(lower, "savanna") {
		return "Savanna Fibre"
	}
	if strings.Contains(lower, "simba") {
		return "Simba Fiber"
	}
	if strings.Contains(lower, "roke") {
		return "ROKE Telkom"
	}
	if strings.Contains(lower, "liquid") {
		return "Liquid Telecom"
	}
	if strings.Contains(lower, "uganda telecom") || strings.Contains(lower, "utl") {
		return "Uganda Telecom (UTL)"
	}
	if strings.Contains(lower, "zuku") {
		return "Zuku Fiber"
	}
	if strings.Contains(lower, "safaricom") {
		return "Safaricom"
	}
	if strings.Contains(lower, "etisalat") || strings.Contains(lower, "e&") {
		return "Etisalat (e&)"
	}
	if strings.Contains(lower, "emirates integrated") || lower == "du" {
		return "du"
	}
	if strings.Contains(lower, "saudi telecom") || strings.Contains(lower, "stc") {
		return "STC"
	}
	if strings.Contains(lower, "mobily") {
		return "Mobily"
	}
	if strings.Contains(lower, "zain") {
		return "Zain"
	}
	if strings.Contains(lower, "ooredoo") {
		return "Ooredoo"
	}
	if strings.Contains(lower, "vodafone") {
		return "Vodafone"
	}
	if strings.Contains(lower, "t-mobile") {
		return "T-Mobile"
	}
	if strings.Contains(lower, "at&t") {
		return "AT&T"
	}
	if strings.Contains(lower, "verizon") {
		return "Verizon"
	}
	if strings.Contains(lower, "cloudflare") {
		return "Cloudflare (CDN)"
	}
	if strings.Contains(lower, "microsoft") {
		return "Microsoft Network"
	}
	if strings.Contains(lower, "google") {
		return "Google Network"
	}
	if strings.Contains(lower, "amazon") || strings.Contains(lower, "aws") {
		return "Amazon AWS"
	}

	cleaned := raw
	for _, suffix := range []string{
		" Limited", " Ltd", " LLC", " Inc.", " Inc", " Corp", " Corporation",
		" Joint-Stock company", " Joint Stock Company", " PJSC", " JSC", " S.A.", " GmbH",
		" Operations Limited", " Services, Inc", " Enterprises, LLC",
	} {
		if idx := strings.Index(strings.ToLower(cleaned), strings.ToLower(suffix)); idx != -1 {
			cleaned = strings.TrimSpace(cleaned[:idx])
		}
	}

	if cleaned == "" {
		return raw
	}
	return cleaned
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
				isp := normalizeISPName(parts[3])
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
					isp = normalizeISPName(apiResp.ISP)
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
			isp := normalizeISPName(info.ISP)
			if isp != "" && isp != "Broadband Provider" && isp != "Local Network" {
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
	limit := 20
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
