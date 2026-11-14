package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Matching/execution layer for asset interception rules. asset_intercept.go only
// stores rules; this file matches a target asset's domain/IP/URL against enabled
// rules. agent tools (add_intent, insert_assets) call it before issuing an intent
// or inserting an asset and reject on a match.

// AssetInterceptKindLabel returns the kind label used in messages to the agent.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "Domain (exact)"
	case "exact_ip":
		return "IP (exact)"
	case "exact_url":
		return "URL (exact)"
	case "fuzzy_domain":
		return "Domain (pattern)"
	case "fuzzy_ip":
		return "IP (pattern)"
	case "fuzzy_url":
		return "URL (pattern)"
	case "cidr":
		return "CIDR subnet"
	}
	return kind
}

// Reason returns a human-readable match reason, e.g. Matched asset interception rule [Domain (pattern): .gov.cn] (note).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("Matched asset interception rule [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += " (" + note + ")"
	}
	return s
}

// matchOne reports whether a single enabled rule matches the given domain/IP/URL candidates,
// returning the matched value.
func matchOne(r AssetInterceptRule, domains, ips, urls []string, allow bool) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.Trim(strings.ToLower(p), ".")
		if lp == "" {
			return "", false
		}
		for _, d := range domains {
			domain := strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
			matched := strings.Contains("."+domain+".", "."+lp+".")
			if allow {
				matched = domain == lp || strings.HasSuffix(domain, "."+lp)
			}
			if matched {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules returns the first enabled rule matching the given
// domain/IP/URL candidates, plus the matched value. insert_assets uses this with
// raw input (assetInputItem not yet persisted).
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	return matchAssetInterceptRules(rules, domains, domains, ips, urls, false)
}

func matchAssetInterceptRules(rules []AssetInterceptRule, domains, exactDomains, ips, urls []string, allow bool) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		ruleDomains := domains
		if allow && r.Kind == "exact_domain" {
			ruleDomains = exactDomains
		}
		if v, ok := matchOne(r, ruleDomains, ips, urls, allow); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates extracts actual domain/IP/URL candidates from a persisted asset.
// URL hosts are classified so domain/IP rules also cover service assets that contain only a URL.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel returns a short asset identifier for messages to the agent.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("Asset #%d [%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule reports whether the rule set contains any enabled rule.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision is the "block first, then allow" gate result for a set of candidates.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // rejection reason (without asset label); empty when Allowed=true
}

// EvaluateAssetGate runs the task-level gate decision:
//  1. A match against any enabled blockRules → deny (interception reason).
//  2. Otherwise, if any allowRules are enabled and none match → deny (outside the allowed scope).
//  3. Otherwise allow.
//
// When allowRules is empty/has no enabled entries the allow gate is inactive (no
// whitelist configured, everything passes), so an unconfigured allowlist never blocks all assets.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	return evaluateAssetGateForCandidates(blockRules, allowRules, domains, domains, ips, urls)
}

func evaluateAssetGateForCandidates(blockRules, allowRules []AssetInterceptRule, domains, exactDomains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := matchAssetInterceptRules(blockRules, domains, exactDomains, ips, urls, false); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := matchAssetInterceptRules(allowRules, domains, exactDomains, ips, urls, true); !ok {
			return AssetGateDecision{Allowed: false, Reason: "not within allowed task scope (whitelist); testing is not permitted"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

func (a *Asset) evaluateAssetGate(blockRules, allowRules []AssetInterceptRule) AssetGateDecision {
	domains, ips, urls := a.interceptCandidates()
	// Preserve root-domain matching except for exact-domain allow rules, which require an actual host.
	allDomains := domains
	if root := strings.TrimSpace(a.RootDomain); root != "" {
		allDomains = append(allDomains, root)
	}
	return evaluateAssetGateForCandidates(blockRules, allowRules, allDomains, domains, ips, urls)
}

// AssetInterceptHit describes an asset rejected by the gate (block match or outside the allowed scope).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // human-readable reason
}

// Describe returns a human-readable description: asset label + reason.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules is a passthrough of the *DB method of the same name so
// callers holding only an AssetStore (e.g. agent tools) can read the rules.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept loads assets by id and runs the "block first, then allow" gate
// on each, returning every rejected asset. Block rules = global ∪ task-level block;
// allow rules = task-level allow (this task only). Returns quickly when ids is empty.
// Uses the global GetByIDs (not filtered by task scope) so interception is not weakened by scope.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// Neither block rules nor an enabled allow rule → no decision needed, everything passes.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		if d := a.evaluateAssetGate(blockRules, allowRules); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
