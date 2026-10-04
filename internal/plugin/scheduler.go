package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-key-billing-plus/internal/billing"
)

const maxPoolsPerKey = 256
const maxCredentialsPerPool = 1024
const noRoutedCredentialMessage = "No available upstream credentials match the routing rules"

type subsetScheduleState struct {
	Current map[string]int64
	Weights map[string]int64
}
type subsetScheduler struct {
	mu   sync.Mutex
	keys map[string]map[string]*subsetScheduleState
}

func candidateWeight(candidate SchedulerAuthCandidate) int64 {
	raw := strings.TrimSpace(candidate.Attributes["weight"])
	if raw == "" && candidate.Metadata["weight"] != nil {
		raw = fmt.Sprint(candidate.Metadata["weight"])
	}
	if raw == "" {
		return 1
	}
	weight, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || weight <= 0 {
		return 0
	}
	return weight
}

func routingPoolKey(model string, decision billing.RoutingDecision) string {
	// Keep round-robin progress separate per model; this does not change which
	// credentials the key is allowed to use.
	policy := decision.RouteRule
	policy.Models, policy.DeniedModels = nil, nil
	raw, _ := json.Marshal(policy)
	sum := sha256.Sum256(raw)
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + hex.EncodeToString(sum[:])
}

func (s *subsetScheduler) pick(scope, pool string, candidates []SchedulerAuthCandidate) string {
	positive := make([]SchedulerAuthCandidate, 0, len(candidates))
	weights := make(map[string]int64, len(candidates))
	for _, candidate := range candidates {
		weight := candidateWeight(candidate)
		if weight > 0 {
			positive = append(positive, candidate)
			weights[candidate.ID] = weight
		}
	}
	if len(positive) == 0 {
		return ""
	}
	if len(positive) == 1 {
		return positive[0].ID
	}
	sort.Slice(positive, func(i, j int) bool { return positive[i].ID < positive[j].ID })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[string]map[string]*subsetScheduleState)
	}
	pools := s.keys[scope]
	if pools == nil {
		pools = make(map[string]*subsetScheduleState)
		s.keys[scope] = pools
	}
	state := pools[pool]
	if state == nil {
		if len(pools) >= maxPoolsPerKey {
			pools = make(map[string]*subsetScheduleState)
			s.keys[scope] = pools
		}
		state = &subsetScheduleState{Current: make(map[string]int64), Weights: make(map[string]int64)}
		pools[pool] = state
	}
	weightsChanged := false
	for _, candidate := range positive {
		weight := weights[candidate.ID]
		if old, ok := state.Weights[candidate.ID]; ok && old != weight {
			weightsChanged = true
		}
		state.Weights[candidate.ID] = weight
	}
	if weightsChanged {
		state.Current = make(map[string]int64)
	}
	if len(state.Current) > maxCredentialsPerPool {
		current := make(map[string]int64, len(positive))
		keptWeights := make(map[string]int64, len(positive))
		for _, c := range positive {
			current[c.ID] = state.Current[c.ID]
			keptWeights[c.ID] = state.Weights[c.ID]
		}
		state.Current = current
		state.Weights = keptWeights
	}
	var selected string
	var highest int64
	total := int64(0)
	for _, candidate := range positive {
		weight := state.Weights[candidate.ID]
		total += weight
		state.Current[candidate.ID] += weight
		score := state.Current[candidate.ID]
		if selected == "" || score > highest || (score == highest && candidate.ID < selected) {
			selected = candidate.ID
			highest = score
		}
	}
	state.Current[selected] -= total
	return selected
}

func (s *subsetScheduler) prune(scopes map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for scope := range s.keys {
		if _, ok := scopes[scope]; !ok {
			delete(s.keys, scope)
		}
	}
}

func candidateAllowed(candidate SchedulerAuthCandidate, decision billing.RoutingDecision) bool {
	return routingAllowsCredential(candidate.ID, credentialSourceFromCandidate(candidate), candidate.Provider, decision)
}

func (a *App) pickCredential(raw []byte) ([]byte, error) {
	a.refreshSessionAffinity()
	var req SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("Parse upstream credential scheduling parameters: %w", err)
	}
	if a == nil || a.store == nil || !a.store.Enabled() {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	if metadataString(req.Options.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	a.observeCandidates(req.Candidates)
	scope := metadataString(req.Options.Metadata, MetadataCallerScope)
	if scope == "" && a.credentialLimit() == 0 {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	requestedModel := metadataString(req.Options.Metadata, MetadataRequestedModel)
	if requestedModel == "" {
		requestedModel = req.Model
	}
	decision := a.store.ResolveRouting(scope, req.Model, requestedModel)
	if decision.ConfigurationError != "" {
		return ErrorEnvelope("routing_configuration_error", decision.ConfigurationError, http.StatusServiceUnavailable), nil
	}
	if !decision.RestrictsCredentials() && a.credentialLimit() == 0 && credentialSessionKey(req, scope) == "" {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	allowed := make([]SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidateAllowed(candidate, decision) {
			allowed = append(allowed, candidate)
		}
	}
	if len(allowed) == 0 {
		return ErrorEnvelope("no_routed_credential", noRoutedCredentialMessage, http.StatusServiceUnavailable), nil
	}
	if a.credentialLimit() > 0 || (a.stickySessionsEnabled() && credentialSessionKey(req, scope) != "") {
		return a.pickCredentialWithLimit(req, scope, decision, allowed)
	}
	if len(allowed) == len(req.Candidates) {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	id := a.scheduler.pick(scope, routingPoolKey(decision.Model, decision), highestPriorityCandidates(allowed))
	if id == "" {
		return ErrorEnvelope("no_routed_credential", noRoutedCredentialMessage, http.StatusServiceUnavailable), nil
	}
	return OKEnvelope(SchedulerPickResponse{AuthID: id, Handled: true})
}

// Reserve and bind under one lock so concurrent first requests cannot create competing bindings.
func (a *App) pickCredentialWithLimit(req SchedulerPickRequest, scope string, decision billing.RoutingDecision, candidates []SchedulerAuthCandidate) ([]byte, error) {
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	requestID := req.Options.Headers.Get(credentialRequestHeader)
	previous, exists := c.active[requestID]
	limit := c.settings.MaxConcurrency
	if limit > 0 && (requestID == "" || !exists) {
		return ErrorEnvelope("invalid_request_id", "Request lifecycle identity is required", 503), nil
	}
	now := time.Now()
	key := ""
	if c.sessionAffinity {
		key = credentialSessionKey(req, scope)
	}
	binding := c.sessions[key]
	sticky := false
	if key != "" && binding.Expires.After(now) {
		for _, candidate := range candidates {
			if candidate.ID == binding.AuthID && candidateWeight(candidate) > 0 {
				candidates = []SchedulerAuthCandidate{candidate}
				sticky = true
				break
			}
		}
	}
	if !sticky {
		delete(c.sessions, key)
	}
	// The configured limit is ordinary capacity; only a live binding may
	// admit requests up to the limit plus the two reserved sticky slots.
	ceiling := limit
	if sticky && limit > 0 {
		ceiling += 2
	}
	available := make([]SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		active := c.counts[candidate.ID]
		if candidate.ID == previous {
			active--
		}
		if candidate.ID != "" && candidateWeight(candidate) > 0 && (limit == 0 || active < ceiling) {
			available = append(available, candidate)
		}
	}
	if len(available) == 0 {
		return ErrorEnvelope("credential_concurrency_limit", "All eligible credentials are at their concurrency limit", 429), nil
	}
	id := a.scheduler.pick(scope, routingPoolKey(decision.Model, decision), highestPriorityCandidates(available))
	if exists && previous != id {
		if previous != "" {
			c.counts[previous]--
		}
		c.counts[id]++
		c.active[requestID] = id
	}
	if c.sessionAffinity && key != "" {
		ttl := c.settings.SessionTTLMinutes
		if ttl < 1 {
			ttl = 5
		}
		c.bindSession(key, id, now, time.Duration(ttl)*time.Minute)
	}
	return OKEnvelope(SchedulerPickResponse{AuthID: id, Handled: true})
}

func (a *App) stickySessionsEnabled() bool {
	a.controls.mu.Lock()
	defer a.controls.mu.Unlock()
	return a.controls.sessionAffinity
}

// Across-priority candidates are needed for affinity, but ordinary weighted selection retains host priority semantics.
func highestPriorityCandidates(candidates []SchedulerAuthCandidate) []SchedulerAuthCandidate {
	priority := -int(^uint(0)>>1) - 1
	for _, candidate := range candidates {
		if candidate.ID == "" || candidateWeight(candidate) <= 0 {
			continue
		}
		if candidate.Priority > priority {
			priority = candidate.Priority
		}
	}
	eligible := make([]SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Priority == priority && candidate.ID != "" && candidateWeight(candidate) > 0 {
			eligible = append(eligible, candidate)
		}
	}
	return eligible
}

type credentialSession struct {
	AuthID  string
	Expires time.Time
}

// Scope, provider namespace and model isolate bindings; only a digest of the client identity is retained.
func credentialSessionKey(req SchedulerPickRequest, scope string) string {
	if scope == "" {
		return ""
	}
	id := metadataString(req.Options.Metadata, "canonical_session_id")
	if id == "" {
		for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Http-Session-Id", "X-Session-ID", "X-Session-Affinity", "X-Slot-Session-Id"} {
			if value := strings.TrimSpace(req.Options.Headers.Get(name)); value != "" {
				id = name + ":" + value
				break
			}
		}
	}
	if id == "" {
		id = metadataString(req.Options.Metadata, "execution_session_id")
	}
	if id == "" {
		return ""
	}
	provider := metadataString(req.Options.Metadata, "session_affinity_provider")
	if provider == "" {
		providers := append([]string(nil), req.Providers...)
		if req.Provider != "" {
			providers = append(providers, req.Provider)
		}
		if len(providers) == 0 {
			seen := map[string]bool{}
			for _, candidate := range req.Candidates {
				if !seen[candidate.Provider] {
					providers = append(providers, candidate.Provider)
					seen[candidate.Provider] = true
				}
			}
		}
		sort.Strings(providers)
		provider = strings.Join(providers, ",")
	}
	model := metadataString(req.Options.Metadata, "session_affinity_model")
	if model == "" {
		model = req.Model
	}
	raw, _ := json.Marshal([]string{scope, provider, model, id})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Bounded synchronous eviction keeps the embedded runtime idle between host calls. Successful admissions refresh the sliding TTL.
func (c *credentialControls) bindSession(key, authID string, now time.Time, ttl time.Duration) {
	if c.sessions == nil {
		c.sessions = make(map[string]credentialSession)
	}
	if _, exists := c.sessions[key]; !exists && len(c.sessions) >= 8192 {
		oldestKey := ""
		var oldest time.Time
		for candidate, session := range c.sessions {
			if !session.Expires.After(now) {
				delete(c.sessions, candidate)
				continue
			}
			if oldestKey == "" || session.Expires.Before(oldest) {
				oldestKey, oldest = candidate, session.Expires
			}
		}
		if len(c.sessions) >= 8192 {
			delete(c.sessions, oldestKey)
		}
	}
	c.sessions[key] = credentialSession{AuthID: authID, Expires: now.Add(ttl)}
}
