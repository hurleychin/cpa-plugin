// account_models.go serves per-account available models + promotion info for
// the panel (GET /models?auth_index=<idx>).
//
// Source: the upstream /v3/config endpoint (same one gateway model discovery
// uses) returns, per account:
//   - data.agents[name==cli].models — model IDs this account can use via CLI;
//   - data.models — full catalog (id, name, descriptionZh, vendor, capabilities);
//   - data.modelPromotions — per-model discount/promo entries (badge label +
//     color, discount factor/text, hover textZh, modelIds, schedule).
//
// Promotion content differs per account (verified: personal 6 entries,
// enterprise 7 entries), so this is fetched per auth, not from the shared
// gateway model cache. Results are cached per authID for 10 minutes; the
// panel lazy-loads one card at a time like /credits does.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// accountModelsCacheTTL bounds reuse of a per-account models/promotions snapshot.
const accountModelsCacheTTL = 10 * time.Minute

type accountModelsEntry struct {
	models  []modelDisplay
	promos  []promoDisplay
	fetched time.Time
}

var accountModelsCache sync.Map // authID -> *accountModelsEntry

// modelDisplay is one account-usable model for the panel.
type modelDisplay struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Desc             string `json:"desc,omitempty"`
	Vendor           string `json:"vendor,omitempty"`
	Rate             string `json:"rate,omitempty"` // credit multiplier, e.g. "x0.79"
	SupportsToolCall bool   `json:"supports_tool_call,omitempty"`
	SupportsImages   bool   `json:"supports_images,omitempty"`
	SupportsReason   bool   `json:"supports_reasoning,omitempty"`
}

// promoDisplay is one model promotion for the panel.
type promoDisplay struct {
	ID       string   `json:"id"`
	Badge    string   `json:"badge,omitempty"`
	Color    string   `json:"color,omitempty"`
	Discount string   `json:"discount,omitempty"` // e.g. "0.50x", "0x"
	Factor   float64  `json:"factor,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Text     string   `json:"text,omitempty"` // hover.textZh
	ModelIDs []string `json:"model_ids,omitempty"`
	Period   string   `json:"period,omitempty"` // human schedule, e.g. "每日23:00–7:50"
	Active   bool     `json:"active,omitempty"`
}

// v3ConfigModels is the subset of /v3/config we need for the panel.
type v3ConfigModels struct {
	Code int `json:"code"`
	Data struct {
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
		Models []struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			DescriptionZh    string `json:"descriptionZh"`
			Vendor           string `json:"vendor"`
			Credits          string `json:"credits"` // credit multiplier, e.g. "x0.79"
			SupportsToolCall bool   `json:"supportsToolCall"`
			SupportsImages   bool   `json:"supportsImages"`
			SupportsReason   bool   `json:"supportsReasoning"`
		} `json:"models"`
		Promotions []v3Promo `json:"modelPromotions"`
	} `json:"data"`
}

// v3Promo mirrors one data.modelPromotions entry.
type v3Promo struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
	Badge    struct {
		Label   string `json:"label"`
		Color   string `json:"color"`
		Display string `json:"display"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
		DisplayMode       string  `json:"displayMode"`
	} `json:"discount"`
	Hover struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	ModelIDs []string   `json:"modelIds"`
	Schedule v3Schedule `json:"schedule"`
}

// v3Schedule mirrors a promotion schedule block.
type v3Schedule struct {
	Timezone string `json:"timezone"`
	Daily    []struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"daily"`
	ValidFrom  string `json:"validFrom"`
	ValidUntil string `json:"validUntil"`
}

// promoLocation resolves the promotion timezone (upstream uses Asia/Shanghai).
func promoLocation(tz string) *time.Location {
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.FixedZone("CST", 8*3600)
}

// parseHM parses "H:MM"/"HH:MM" into minutes since midnight.
func parseHM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// promoActive reports whether the promotion is in effect now.
func promoActive(p v3Promo, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	loc := promoLocation(p.Schedule.Timezone)
	local := now.In(loc)
	if p.Schedule.ValidFrom != "" {
		from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom)
		if err != nil || local.Before(from.In(loc)) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil)
		if err != nil || !local.Before(until.In(loc)) {
			return false
		}
	}
	if len(p.Schedule.Daily) == 0 {
		return true
	}
	cur := local.Hour()*60 + local.Minute()
	for _, w := range p.Schedule.Daily {
		s, ok1 := parseHM(w.Start)
		e, ok2 := parseHM(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if s <= e {
			if cur >= s && cur < e {
				return true
			}
		} else if cur >= s || cur < e { // overnight window, e.g. 23:00–7:50
			return true
		}
	}
	return false
}

// promoPeriod renders a human schedule string.
func promoPeriod(p v3Promo) string {
	var parts []string
	for _, w := range p.Schedule.Daily {
		s := strings.TrimSpace(w.Start)
		e := strings.TrimSpace(w.End)
		if s == "" || e == "" {
			continue
		}
		// Full-day informational badges carry no real period.
		if s == "0:00" && (e == "23:59" || e == "24:00") {
			continue
		}
		parts = append(parts, "每日"+s+"–"+e)
	}
	period := strings.Join(parts, "、")
	if p.Schedule.ValidFrom != "" || p.Schedule.ValidUntil != "" {
		loc := promoLocation(p.Schedule.Timezone)
		dates := ""
		if p.Schedule.ValidFrom != "" {
			if from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom); err == nil {
				dates = from.In(loc).Format("1月2日")
			}
		}
		if p.Schedule.ValidUntil != "" {
			if until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil); err == nil {
				// ValidUntil is exclusive (e.g. 10-01T00:00 = ends 9-30).
				end := until.In(loc).Add(-time.Second)
				if dates != "" {
					dates += "–"
				}
				dates += end.Format("1月2日")
			}
		}
		if dates != "" {
			if period != "" {
				period += "（" + dates + "）"
			} else {
				period = dates
			}
		}
	}
	return period
}

// fetchV3ConfigModels GETs /v3/config (realm-aware) and parses the panel subset.
func fetchV3ConfigModels(sa *storedAuth) (*v3ConfigModels, error) {
	isGlobal := isGlobalToken(sa.Auth.AccessToken)
	modelsURL := upstreamBaseCN + "/v3/config"
	if isGlobal {
		modelsURL = upstreamBaseGlobal + "/v3/config"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Connection", "close")
	req.Header.Set("User-Agent", "WorkBuddy/5.3.13 WorkBuddy/5.3.13 CLI/2.115.0")
	req.Header.Set("X-Request-ID", randomHex(16))
	req.Header.Set("X-Product", "SaaS")
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", sa.Account.EnterpriseID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models config status %d", resp.StatusCode)
	}
	var cfg v3ConfigModels
	if err := json.Unmarshal(resp.Body, &cfg); err != nil {
		return nil, err
	}
	if cfg.Code != 0 {
		return nil, fmt.Errorf("models config code %d", cfg.Code)
	}
	return &cfg, nil
}

// getAccountModels returns cached (or freshly fetched) per-account models +
// promotions. Enterprise custom:* models are appended for enterprise accounts.
func getAccountModels(authID string, sa *storedAuth, force bool) ([]modelDisplay, []promoDisplay, error) {
	if !force {
		if v, ok := accountModelsCache.Load(authID); ok {
			if e, ok2 := v.(*accountModelsEntry); ok2 && time.Since(e.fetched) < accountModelsCacheTTL {
				return e.models, e.promos, nil
			}
		}
	}
	cfg, err := fetchV3ConfigModels(sa)
	if err != nil {
		return nil, nil, err
	}
	catalog := make(map[string]modelDisplay, len(cfg.Data.Models))
	for _, m := range cfg.Data.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = id
		}
		catalog[id] = modelDisplay{
			ID: id, Name: name, Desc: m.DescriptionZh, Vendor: m.Vendor,
			Rate:             strings.TrimSpace(m.Credits),
			SupportsToolCall: m.SupportsToolCall, SupportsImages: m.SupportsImages,
			SupportsReason: m.SupportsReason,
		}
	}
	var cliIDs []string
	for _, a := range cfg.Data.Agents {
		if a.Name == "cli" {
			cliIDs = a.Models
			break
		}
	}
	var models []modelDisplay
	seen := make(map[string]struct{}, len(cliIDs))
	for _, id := range cliIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if mi, ok := catalog[id]; ok {
			models = append(models, mi)
		} else {
			models = append(models, modelDisplay{ID: id, Name: id})
		}
	}
	// Enterprise custom models (personal accounts: call returns error, ignored).
	if ems, err := callEnterpriseModelsAPI(sa); err == nil {
		for _, m := range ems {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			models = append(models, modelDisplay{ID: m.ID, Name: m.Name})
		}
	}
	now := time.Now()
	var promos []promoDisplay
	for _, p := range cfg.Data.Promotions {
		if !p.Enabled || strings.TrimSpace(p.ID) == "" {
			continue
		}
		pd := promoDisplay{
			ID: p.ID, Badge: p.Badge.Label, Color: p.Badge.Color,
			Text: p.Hover.TextZh, ModelIDs: p.ModelIDs, Priority: p.Priority,
			Period: promoPeriod(p), Active: promoActive(p, now),
		}
		if p.Discount != nil {
			pd.Discount = p.Discount.DiscountedCredits
			pd.Factor = p.Discount.Factor
		}
		promos = append(promos, pd)
	}
	accountModelsCache.Store(authID, &accountModelsEntry{models: models, promos: promos, fetched: now})
	return models, promos, nil
}

// handleAccountModels serves GET /models?auth_index=<idx>[&refresh=1]: the
// account's usable models + model promotions for the panel card.
func handleAccountModels(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	force := false
	if vals := req.Query["refresh"]; len(vals) > 0 {
		v := strings.TrimSpace(vals[0])
		force = v == "1" || strings.EqualFold(v, "true")
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": "load auth: " + err.Error()}
		}
		models, promos, err := getAccountModels(f.ID, sa, force)
		if err != nil {
			return map[string]any{"auth_index": authIndex, "error": err.Error()}
		}
		if models == nil {
			models = []modelDisplay{}
		}
		if promos == nil {
			promos = []promoDisplay{}
		}
		return map[string]any{
			"auth_index": authIndex,
			"models":     models,
			"promotions": promos,
		}
	}
	return map[string]any{"error": "account not found"}
}
