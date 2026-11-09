package main

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type rateLimits struct {
	Pro      *float64 `json:"remaining_pro"`
	Research *float64 `json:"remaining_research"`
	Labs     *float64 `json:"remaining_labs"`
	Agentic  *float64 `json:"remaining_agentic_research"`
}

type userSettings struct {
	Uploads *float64 `json:"upload_limit"`
	Pages   *float64 `json:"pages_limit"`
}

func normalizeQuota(tier string, rates rateLimits, user userSettings, cfg settings) pluginapi.QuotaFetchResponse {
	result := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: tier}, Groups: []pluginapi.QuotaGroup{{DisplayName: "Perplexity"}}}
	metrics := []struct {
		key   string
		label string
		value *float64
	}{
		{"pro_searches_remaining", "Pro searches remaining", rates.Pro},
		{"research_remaining", "Research remaining", rates.Research},
		{"labs_remaining", "Labs remaining", rates.Labs},
		{"agentic_research_remaining", "Agentic research remaining", rates.Agentic},
		{"uploads_remaining", "Uploads remaining", user.Uploads},
		{"pages_remaining", "Pages remaining", user.Pages},
	}
	for _, metric := range metrics {
		if metric.value != nil {
			result.Summary = append(result.Summary, pluginapi.QuotaMetric{Key: metric.key, Label: metric.label, Value: *metric.value, Format: "number"})
		}
	}
	windows := []struct {
		name      string
		remaining *float64
		limit     int
	}{{"pro", rates.Pro, cfg.ProQueryLimit}, {"research", rates.Research, cfg.ResearchQueryLimit}}
	for _, window := range windows {
		if window.limit > 0 && window.remaining != nil {
			result.Groups[0].Buckets = append(result.Groups[0].Buckets, pluginapi.QuotaBucket{Window: window.name, RemainingFraction: *window.remaining / float64(window.limit), Description: fmt.Sprintf("%g / %d %s queries remaining (configured limit)", *window.remaining, window.limit, window.name)})
		}
	}
	return result
}

func (rt *pluginRuntime) fetchQuota(request pluginapi.QuotaFetchRequest, id string, callback hostCallbackFunc) (pluginapi.QuotaFetchResponse, error) {
	cred, err := parseCredential(request.StorageJSON)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	rt.rememberHost(request.Host)
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(min(cfg.Timeout, 8*time.Second))
	defer cancel()
	client := sender{callback: callback, id: id, cfg: cfg}
	validated, err := probeSession(ctx, client, cred)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	var rates rateLimits
	if err = client.getJSON(ctx, "/rest/rate-limit/all", &validated, &rates); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	var user userSettings
	if err = client.getJSON(ctx, "/rest/user/settings", &validated, &user); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	var credits any
	_ = client.getJSON(ctx, "/rest/billing/credits", &validated, &credits)
	return normalizeQuota(validated.Plan, rates, user, cfg), nil
}
