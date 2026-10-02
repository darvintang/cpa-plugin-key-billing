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
	if !decision.RestrictsCredentials() && a.credentialLimit() == 0 {
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
	if a.credentialLimit() > 0 {
		return a.pickCredentialWithLimit(req, scope, decision, allowed)
	}
	if len(allowed) == len(req.Candidates) {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	id := a.scheduler.pick(scope, routingPoolKey(decision.Model, decision), allowed)
	if id == "" {
		return ErrorEnvelope("no_routed_credential", noRoutedCredentialMessage, http.StatusServiceUnavailable), nil
	}
	return OKEnvelope(SchedulerPickResponse{AuthID: id, Handled: true})
}

// Reserve under one lock, so simultaneous schedulers cannot exceed the shared per-credential ceiling.
func (a *App) pickCredentialWithLimit(req SchedulerPickRequest, scope string, decision billing.RoutingDecision, candidates []SchedulerAuthCandidate) ([]byte, error) {
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	requestID := req.Options.Headers.Get(credentialRequestHeader)
	previous, exists := c.active[requestID]
	if requestID == "" || !exists {
		return ErrorEnvelope("invalid_request_id", "Request lifecycle identity is required", 503), nil
	}
	available := make([]SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		active := c.counts[candidate.ID]
		if candidate.ID == previous {
			active--
		}
		if candidate.ID != "" && candidateWeight(candidate) > 0 && (c.settings.MaxConcurrency == 0 || active < c.settings.MaxConcurrency) {
			available = append(available, candidate)
		}
	}
	if len(available) == 0 {
		return ErrorEnvelope("credential_concurrency_limit", "All eligible credentials are at their concurrency limit", 429), nil
	}
	priority := available[0].Priority
	for _, candidate := range available {
		if candidate.Priority > priority {
			priority = candidate.Priority
		}
	}
	eligible := available[:0]
	for _, candidate := range available {
		if candidate.Priority == priority {
			eligible = append(eligible, candidate)
		}
	}
	id := a.scheduler.pick(scope, routingPoolKey(decision.Model, decision), eligible)
	if previous != id {
		if previous != "" {
			c.counts[previous]--
		}
		c.counts[id]++
		c.active[requestID] = id
	}
	return OKEnvelope(SchedulerPickResponse{AuthID: id, Handled: true})
}
