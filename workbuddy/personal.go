// personal.go implements the personal-edition (个人版) support ported from
// upstream (Sliverkiss/cpa-plugin origin/main): daily check-in for CN
// accounts (once daily at a random time in 09:00~10:00 + manual /checkin), expert trial-pack
// claim for Global accounts (/trial), personal resource-package credits, and
// the check-in panel state.
//
// Enterprise isolation (this fork's v0.8.7+ direction is intentionally kept):
//   - isEnterpriseAccount(sa) == EnterpriseID != "". Enterprise accounts NEVER
//     touch personal endpoints: fetchUserResource routes them to the
//     enterprise usage API only (billing.go, unchanged behavior), the auto
//     scheduler skips check-in for them, and manual /checkin reports
//     skipped/enterprise.
//   - Personal accounts (EnterpriseID == "") use the upstream personal APIs
//     verbatim: /v2/billing/meter/get-user-resource (paged),
//     /v2/billing/meter/checkin-activity-status, /v2/billing/meter/daily-checkin,
//     /billing/ide/trial.
//
// Adapted from upstream by dropping the host-callback variants (this fork's
// host bridge exposes hostHTTPDo without callback propagation) — behavior is
// otherwise identical.
package main

import (
	"encoding/json"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Personal auto check-in runs once daily at a random time inside the
// 09:00~10:00 local window. The day's target is derived deterministically
// from the date (FNV hash → second offset), so restarts on the same day keep
// the same target: no double check-in, while the time still varies
// unpredictably day to day.
const (
	checkinWindowStartHour = 9
	checkinWindowMinutes   = 60
)

// checkinScheduleLabel is surfaced in the dashboard for the panel.
const checkinScheduleLabel = "09:00~10:00（每日随机）"

var (
	lastCheckinTickDayMu sync.Mutex
	lastCheckinTickDay   string // "2006-01-02" of the last scheduler tick inside the window
)

// personalCheckinWindow returns today's [09:00, 10:00) local window.
func personalCheckinWindow(now time.Time) (start, end time.Time) {
	start = time.Date(now.Year(), now.Month(), now.Day(), checkinWindowStartHour, 0, 0, 0, now.Location())
	return start, start.Add(time.Duration(checkinWindowMinutes) * time.Minute)
}

// personalCheckinOffset returns the deterministic per-day offset in [0, 60min)
// from the date hash.
func personalCheckinOffset(day time.Time) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(day.Format("2006-01-02")))
	return time.Duration(h.Sum32()%3600) * time.Second
}

// personalCheckinTarget returns the day's random target time (09:00 + offset).
func personalCheckinTarget(day time.Time) time.Time {
	start, _ := personalCheckinWindow(day)
	return start.Add(personalCheckinOffset(day))
}

// inPersonalCheckinWindow reports whether now falls in today's window.
func inPersonalCheckinWindow(now time.Time) bool {
	start, end := personalCheckinWindow(now)
	return !now.Before(start) && now.Before(end)
}

// markPersonalCheckinTick records that the scheduler fired inside today's
// window (regardless of outcome) so the next tick waits for tomorrow.
func markPersonalCheckinTick(now time.Time) {
	lastCheckinTickDayMu.Lock()
	lastCheckinTickDay = now.Format("2006-01-02")
	lastCheckinTickDayMu.Unlock()
}

func personalCheckinTickedToday(now time.Time) bool {
	lastCheckinTickDayMu.Lock()
	defer lastCheckinTickDayMu.Unlock()
	return lastCheckinTickDay == now.Format("2006-01-02")
}

// nextPersonalCheckinTime returns the next scheduler wake-up for the personal
// tick: today's random target if still ahead, ASAP when inside the window
// with no tick yet (covers restarts), otherwise tomorrow's target.
// runAutoCheckin itself skips already-checked-in accounts, so ASAP firing is
// idempotent-safe.
func nextPersonalCheckinTime(now time.Time) time.Time {
	if personalCheckinTickedToday(now) {
		return personalCheckinTarget(now.Add(24 * time.Hour))
	}
	if target := personalCheckinTarget(now); target.After(now) {
		return target
	}
	if inPersonalCheckinWindow(now) {
		return now
	}
	return personalCheckinTarget(now.Add(24 * time.Hour))
}

// checkinAuto gates the daily auto check-in. Default true; configurable via
// plugin config key "checkin_auto".
var (
	checkinAuto   = true
	checkinAutoMu sync.RWMutex
)

func checkinAutoEnabled() bool {
	checkinAutoMu.RLock()
	defer checkinAutoMu.RUnlock()
	return checkinAuto
}

// isEnterpriseAccount reports whether the credential belongs to an enterprise
// org account. Enterprise quotas are granted by the org, not by personal
// check-in, so all personal-edition flows skip these accounts.
func isEnterpriseAccount(sa *storedAuth) bool {
	return sa != nil && strings.TrimSpace(sa.Account.EnterpriseID) != ""
}

// shouldFetchCheckin reports whether a check-in status probe makes sense for
// the account: personal CN only (Global uses trial claims; enterprise uses org
// quota).
func shouldFetchCheckin(sa *storedAuth) bool {
	if sa == nil || isEnterpriseAccount(sa) {
		return false
	}
	return !isGlobalDomain(sa.Auth.Domain)
}

// checkinSummary mirrors the upstream check-in status shape.
type checkinSummary struct {
	Active          bool     `json:"active"`
	TodayCheckedIn  bool     `json:"today_checked_in"`
	StreakDays      int64    `json:"streak_days"`
	DailyCredit     int64    `json:"daily_credit"`
	TodayCredit     int64    `json:"today_credit"`
	TotalCredits    int64    `json:"total_credits"`
	WeekCheckinDays int64    `json:"week_checkin_days"`
	ActivityName    string   `json:"activity_name"`
	Season          int64    `json:"season"`
	CheckinDates    []string `json:"checkin_dates,omitempty"`
}

// resourcePackage mirrors the upstream get-user-resource Accounts[] shape.
// Capacity* — lifetime package totals; CycleCapacity* — active billing cycle.
type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleStartTime      string `json:"CycleStartTime"`
	CycleEndTime        string `json:"CycleEndTime"`
}

// Concurrency gate for the paged personal resource fetch (upstream parity:
// was unbounded fan-out under dashboard + scheduler concurrency).
const userResourceConcurrency = 4

var userResourceSlots = make(chan struct{}, userResourceConcurrency)

func acquireUserResourceSlot() func() {
	userResourceSlots <- struct{}{}
	return func() { <-userResourceSlots }
}

// -----------------------------------------------------------------------------
// Personal billing API calls
// -----------------------------------------------------------------------------

func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	var data json.RawMessage
	var lastErr error
	for _, path := range []string{"/v2/billing/meter/checkin-activity-status", "/v2/billing/meter/checkin-status"} {
		d, err := billingCall(sa, path, nil)
		if err == nil {
			data = d
			lastErr = nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	sum := &checkinSummary{
		Active:          jsonBool(m, "active", "Active"),
		TodayCheckedIn:  jsonBool(m, "today_checked_in", "todayCheckedIn"),
		StreakDays:      jsonI64(m, "streak_days", "streakDays"),
		DailyCredit:     jsonI64(m, "daily_credit", "dailyCredit"),
		TodayCredit:     jsonI64(m, "today_credit", "todayCredit"),
		TotalCredits:    jsonI64(m, "total_credits", "totalCredits"),
		WeekCheckinDays: jsonI64(m, "week_checkin_days", "weekCheckinDays"),
		ActivityName:    jsonStr(m, "activity_name", "activityName"),
		Season:          jsonI64(m, "season", "season"),
	}
	if dates, ok := m["checkin_dates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	} else if dates, ok := m["checkinDates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	}
	return sum, nil
}

// packageRemainUsed picks current-cycle remain/used/size for one package.
// Prefer cycle metrics whenever CycleCapacitySize is present; used = size−remain
// so missing CycleCapacityUsed never under-reports consumption.
//
// Daily check-in adds NEW packages (size grows) — capacity grant, not negative
// consumption. Track consumption via used (size−remain), not via remain alone.
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	if a.CycleCapacityRemain > 0 || a.CycleCapacityUsed > 0 {
		remain = a.CycleCapacityRemain
		used = a.CycleCapacityUsed
		if remain < 0 {
			remain = 0
		}
		if used < 0 {
			used = 0
		}
		size = remain + used
		if a.CapacitySize > size {
			size = a.CapacitySize
			if size >= remain {
				used = size - remain
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if remain < 0 {
		remain = 0
	}
	if used < 0 {
		used = 0
	}
	if size <= 0 {
		size = remain + used
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// fetchPersonalUserResource aggregates ALL personal resource packages
// (体验版 + 签到/裂变包 + 其它赠送包). Personal-only; enterprise accounts never
// reach here (see fetchUserResource routing in billing.go).
func fetchPersonalUserResource(sa *storedAuth) (*creditsSummary, error) {
	release := acquireUserResourceSlot()
	defer release()

	now := time.Now()
	// Status 0=active, 3=exhausted-but-still-listed.
	const pageSize = 100
	baseBody := map[string]any{
		"PageSize":                 pageSize,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	var all []resourcePackage
	var totalCount int64
	var totalDosage int64
	for page := 1; ; page++ {
		body := make(map[string]any, len(baseBody)+1)
		for k, v := range baseBody {
			body[k] = v
		}
		body["PageNumber"] = page
		data, err := billingCall(sa, "/v2/billing/meter/get-user-resource", body)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				Data struct {
					TotalCount  int64             `json:"TotalCount"`
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, err
		}
		if page == 1 {
			totalCount = resp.Response.Data.TotalCount
			totalDosage = resp.Response.Data.TotalDosage
		}
		all = append(all, resp.Response.Data.Accounts...)
		if len(resp.Response.Data.Accounts) < pageSize || (totalCount > 0 && totalCount <= int64(len(all))) {
			break
		}
	}
	sum := &creditsSummary{}
	for _, a := range all {
		remain, used, size := packageRemainUsed(a)
		sum.TotalRemain += remain
		sum.TotalUsed += used
		sum.TotalSize += size
		sum.Packages = append(sum.Packages, packageSummary{
			Name:       a.PackageName,
			Remain:     remain,
			Used:       used,
			Size:       size,
			CycleStart: a.CycleStartTime,
			CycleEnd:   a.CycleEndTime,
		})
	}
	sum.PackCount = len(sum.Packages)
	if sum.TotalSize > 0 {
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	// Upstream TotalDosage is the capacity pool, not spend — size floor only.
	if totalDosage > sum.TotalSize {
		sum.TotalSize = totalDosage
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	return sum, nil
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	data, err := billingCall(sa, "/v2/billing/meter/daily-checkin", nil)
	if err != nil {
		// Business errors (code != 0) surface as structured results so the
		// panel can show "already checked in".
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["success"] = true
	return m, nil
}

// performTrialCall claims the one-time expert trial pack for a Global personal
// account. POST /billing/ide/trial (note: NOT under /v2/billing/meter/).
// Repeat call: code=14051 "has applied trial" — surfaced as already_claimed.
func performTrialCall(sa *storedAuth) (map[string]any, error) {
	data, err := billingCall(sa, "/billing/ide/trial", nil)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "14051") {
			return map[string]any{
				"success":         false,
				"message":         "已领取过专家加油包",
				"already_claimed": true,
			}, nil
		}
		return map[string]any{"success": false, "message": msg}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["success"] = true
	return m, nil
}

// hasTrialPack reports whether the credits summary already contains the Global
// expert trial pack. Do NOT match bare Chinese "体验": CN free-tier is
// literally named "CodeBuddy个人体验版"/"体验版" and must stay unclaimed-looking.
func hasTrialPack(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	for _, p := range cr.Packages {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name == "" {
			continue
		}
		if strings.Contains(name, "trial") {
			return true
		}
		if strings.Contains(name, "pro plan") && (strings.Contains(name, "free") || strings.Contains(name, "one-time") || strings.Contains(name, "2-week") || strings.Contains(name, "2 week")) {
			return true
		}
		if strings.Contains(name, "专家加油") || strings.Contains(name, "专家体验包") {
			return true
		}
	}
	return false
}

// cachedCheckinToday returns cached today_checked_in when present.
func cachedCheckinToday(authID string) *bool {
	v, ok := accountCache.Load(authID)
	if !ok {
		return nil
	}
	e, ok := v.(*accountCacheEntry)
	if !ok || e == nil || e.checkin == nil {
		return nil
	}
	b := e.checkin.TodayCheckedIn
	return &b
}

// -----------------------------------------------------------------------------
// Per-account check-in locks (personal flows only; enterprise lifecycle keeps
// using authLocks in checkin.go).
// -----------------------------------------------------------------------------

var checkinLocks sync.Map // auth_index -> *sync.Mutex

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func withCheckinLock(authIndex string, fn func()) {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

// pruneCheckinLocks removes lock entries for auth indices that no longer exist.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{}
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}

// mergeCheckinCache stores the latest check-in snapshot while preserving the
// credits/plan fields of any previous cache entry (merge, not replace).
func mergeCheckinCache(authID string, ci *checkinSummary) {
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev, _ = v.(*accountCacheEntry)
	}
	entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
	if prev != nil {
		entry.credits = prev.credits
		entry.plan = prev.plan
	}
	accountCache.Store(authID, entry)
}

// -----------------------------------------------------------------------------
// Auto check-in scheduler tick (personal CN only)
// -----------------------------------------------------------------------------

// runAutoCheckin is the scheduled personal tick (once daily, random time in
// 09:00~10:00; see nextPersonalCheckinTime).
// Enterprise accounts are skipped for check-in (lifecycle still applies);
// Global personal accounts get lifecycle only (trial is manual one-shot).
func runAutoCheckin() {
	doCheckin := checkinAutoEnabled()
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
}

func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	if !doCheckin {
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		return
	}
	if isEnterpriseAccount(sa) || isGlobalDomain(sa.Auth.Domain) {
		// Enterprise/Global: never check-in. Invalidate credits so the next
		// read refetches, then run lifecycle.
		if v, ok := accountCache.Load(f.ID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(f.ID, &fresh)
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	withCheckinLock(f.AuthIndex, func() {
		ci, err := fetchCheckinStatus(sa)
		if err == nil && ci != nil && ci.Active && !ci.TodayCheckedIn {
			if _, callErr := performCheckinCall(sa); callErr == nil {
				if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
					ci = ci2
				}
				if cr2, crErr := fetchUserResource(sa); crErr == nil && cr2 != nil {
					if v, ok := accountCache.Load(f.ID); ok {
						if prev, ok2 := v.(*accountCacheEntry); ok2 {
							fresh := *prev
							fresh.credits = cr2
							fresh.fetched = time.Now()
							accountCache.Store(f.ID, &fresh)
						}
					}
				}
			}
		}
		if ci != nil {
			var prev *accountCacheEntry
			if v, ok := accountCache.Load(f.ID); ok {
				prev, _ = v.(*accountCacheEntry)
			}
			entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
			if prev != nil {
				entry.credits = prev.credits
				entry.plan = prev.plan
			}
			accountCache.Store(f.ID, entry)
		}
	})
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// -----------------------------------------------------------------------------
// Manual management endpoints
// -----------------------------------------------------------------------------

// handleManualCheckin serves POST /checkin. Single-account mode runs one
// check-in with minimal round-trips; batch mode fans out (sem=4) preserving
// input order and the {results, summary} shape the panel depends on.
// Enterprise accounts report skipped/enterprise; Global reports skipped/global.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = checkinOneAccount(f)
		}(i, f)
	}
	wg.Wait()

	successN, failN, alreadyN, globalN, eligibleN := 0, 0, 0, 0, 0
	for _, out := range results {
		if out == nil {
			continue
		}
		if out["error"] != nil {
			failN++
			continue
		}
		reason, _ := out["reason"].(string)
		switch reason {
		case "already":
			alreadyN++
			continue
		case "global", "enterprise":
			globalN++
			continue
		}
		eligibleN++
		if out["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":          len(targets),
			"eligible":       eligibleN,
			"success":        successN,
			"already":        alreadyN,
			"skipped_global": globalN,
			"fail":           failN,
			"attempted":      eligibleN,
		},
	}
}

func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	out := map[string]any{"auth_index": f.AuthIndex}

	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname

	if isEnterpriseAccount(sa) {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "enterprise"
		out["message"] = "企业版账号按组织配额结算，无需签到"
		return out
	}
	if isGlobalDomain(sa.Auth.Domain) {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "global"
		out["message"] = "国际版账号不支持签到，请使用领取专家加油包"
		return out
	}

	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()

	ci, ciErr := fetchCheckinStatus(sa)
	if ciErr == nil && ci != nil && ci.TodayCheckedIn {
		mergeCheckinCache(f.ID, ci)
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "already checked in today"
		return out
	}

	res, err := performCheckinCall(sa)
	if err != nil {
		out["error"] = err.Error()
		out["success"] = false
		return out
	}
	for k, v := range res {
		out[k] = v
	}
	if msg, _ := out["message"].(string); msg != "" && out["success"] == false {
		low := strings.ToLower(msg)
		if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
			out["success"] = true
			out["skipped"] = true
			out["reason"] = "already"
		}
	}
	if _, ok := out["success"]; !ok {
		out["success"] = true
	}

	if ci2, err2 := fetchCheckinStatus(sa); err2 == nil && ci2 != nil {
		mergeCheckinCache(f.ID, ci2)
	} else {
		mergeCheckinCache(f.ID, &checkinSummary{TodayCheckedIn: true})
	}
	return out
}

// handleCheckinConfig serves POST /checkin/config. Runtime-only toggle: the
// host exposes no plugin-config write callback, so the config_yaml value wins
// again on restart.
func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	checkinAutoMu.Lock()
	if body.Enabled != nil {
		checkinAuto = *body.Enabled
	}
	cur := checkinAuto
	checkinAutoMu.Unlock()
	return map[string]any{"checkin_auto": cur, "persistent": false}
}

// handleClaimTrial serves POST /trial: claims the expert trial pack for one
// Global personal account. CN and enterprise accounts are rejected.
func handleClaimTrial(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
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
			return map[string]any{"auth_index": authIndex, "error": err.Error()}
		}
		if isEnterpriseAccount(sa) {
			return map[string]any{"auth_index": authIndex, "error": "企业版账号无需领取专家加油包"}
		}
		if !isGlobalDomain(sa.Auth.Domain) {
			return map[string]any{"auth_index": authIndex, "error": "专家加油包仅适用于国际版账号"}
		}
		res, err := performTrialCall(sa)
		out := map[string]any{"auth_index": authIndex, "nickname": sa.Account.Nickname}
		if err != nil {
			out["error"] = err.Error()
		} else {
			for k, v := range res {
				out[k] = v
			}
		}
		if v, ok := accountCache.Load(f.ID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(f.ID, &fresh)
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(authIndex, f.ID, true)
		}
		return out
	}
	return map[string]any{"error": "account not found"}
}
