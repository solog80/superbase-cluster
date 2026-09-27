package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type GeoInfo struct {
	Query         string  `json:"query"`
	Status        string  `json:"status"`
	Continent     string  `json:"continent"`
	ContinentCode string  `json:"continentCode"`
	CountryName   string  `json:"country"`
	CountryCode   string  `json:"countryCode"`
	Region        string  `json:"region"`
	RegionName    string  `json:"regionName"`
	City          string  `json:"city"`
	District      string  `json:"district"`
	Zip           string  `json:"zip"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
	Timezone      string  `json:"timezone"`
	Offset        int     `json:"offset"`
	Currency      string  `json:"currency"`
	ISP           string  `json:"isp"`
	Org           string  `json:"org"`
	AS            string  `json:"as"`
	ASName        string  `json:"asname"`
	Mobile        bool    `json:"mobile"`
	Proxy         bool    `json:"proxy"`
	IsDatacenter  bool    `json:"hosting"`
}

type GeoIPService struct {
	mu         sync.RWMutex
	cache      map[string]GeoInfo
	httpClient *http.Client
}

var globalGeoIPService = newGeoIPService()

func newGeoIPService() *GeoIPService {
	return &GeoIPService{
		cache: make(map[string]GeoInfo),
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
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
			Query:         ipStr,
			Status:        "success",
			CountryCode:   "UG",
			CountryName:   "Uganda",
			City:          "Kampala",
			Region:        "C",
			RegionName:    "Central Region",
			Lat:           0.31628,
			Lon:           32.58219,
			Timezone:      "Africa/Kampala",
			ISP:           "Local Network",
			IsDatacenter:  false,
		}
	}

	s.mu.RLock()
	info, found := s.cache[ipStr]
	s.mu.RUnlock()
	if found {
		return info
	}

	apiURL := fmt.Sprintf("http://ip-api.com/json/%s?fields=66846719", ipStr)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err == nil {
		resp, err := s.httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				var apiResp GeoInfo
				if err := json.Unmarshal(body, &apiResp); err == nil && apiResp.Status == "success" {
					info = apiResp
					if info.Query == "" {
						info.Query = ipStr
					}
					info.ISP = normalizeISPName(apiResp.ISP)
				}
			}
		}
	}

	if info.CountryCode == "" {
		info.CountryCode = "UG"
	}
	if info.CountryName == "" {
		info.CountryName = "Uganda"
	}
	if info.ISP == "" {
		info.ISP = "Broadband Provider"
	}

	s.mu.Lock()
	s.cache[ipStr] = info
	s.mu.Unlock()

	return info
}

// GetCountryISPBreakdown dynamically aggregates real ISP frequencies for a given country
// from the loaded GeoIP cache and scales them to totalViewers.
func (s *GeoIPService) GetCountryISPBreakdown(cName string, totalViewers int) []ispItem {
	code := getIsoCode(cName)
	cNameLower := strings.ToLower(strings.TrimSpace(cName))

	s.mu.RLock()
	counts := make(map[string]int)
	totalKnownIPs := 0
	for _, info := range s.cache {
		if info.IsDatacenter {
			continue
		}
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

// GetCountryCityBreakdown dynamically aggregates real city frequencies for a given country
// from the loaded GeoIP cache and scales them to totalViewers.
func (s *GeoIPService) GetCountryCityBreakdown(cName string, totalViewers int) []cityItem {
	code := getIsoCode(cName)
	cNameLower := strings.ToLower(strings.TrimSpace(cName))

	s.mu.RLock()
	counts := make(map[string]int)
	totalKnownIPs := 0
	for _, info := range s.cache {
		if info.IsDatacenter {
			continue
		}
		if strings.ToLower(strings.TrimSpace(info.CountryName)) == cNameLower || strings.ToLower(strings.TrimSpace(info.CountryCode)) == cNameLower {
			city := info.City
			if city != "" && city != "-" {
				counts[city]++
				totalKnownIPs++
			}
		}
	}
	s.mu.RUnlock()

	if totalKnownIPs == 0 || len(counts) == 0 {
		return []cityItem{}
	}

	type kv struct {
		city  string
		count int
	}
	var sorted []kv
	for k, v := range counts {
		sorted = append(sorted, kv{city: k, count: v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	var items []cityItem
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
		items = append(items, cityItem{
			Code:    code,
			Country: cName,
			City:    sorted[i].city,
			Viewers: v,
		})
	}
	if len(items) > 0 && allocated < totalViewers {
		items[0].Viewers += (totalViewers - allocated)
	}
	return items
}

// BackfillIPs resolves a list of unique client IP addresses via ip-api.com
// and caches their City, Region, ISP, and Datacenter status in memory.
func (s *GeoIPService) BackfillIPs(ips []string) int {
	var toLookup []string
	s.mu.RLock()
	for _, ip := range ips {
		ip = sanitizeIP(ip)
		if ip == "" || ip == "-" || strings.HasPrefix(ip, "172.") || strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") {
			continue
		}
		if _, found := s.cache[ip]; !found {
			toLookup = append(toLookup, ip)
		}
	}
	s.mu.RUnlock()

	if len(toLookup) == 0 {
		return 0
	}

	log.Printf("[GeoIP] Backfilling %d unique IP addresses via ip-api.com...", len(toLookup))
	count := 0
	for _, ip := range toLookup {
		s.Lookup(ip)
		count++
		if count%40 == 0 {
			time.Sleep(1 * time.Second)
		}
	}
	log.Printf("[GeoIP] Backfill completed. Cache now contains %d resolved IP locations.", len(s.cache))
	return count
}
