package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type upstreamModel struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
	Provider    string `json:"provider"`
}

type searchModel struct {
	Label             string  `json:"label"`
	Description       string  `json:"description"`
	SubscriptionTier  *string `json:"subscription_tier"`
	NonReasoningModel string  `json:"non_reasoning_model"`
	ReasoningModel    string  `json:"reasoning_model"`
	IsDefault         bool    `json:"is_default"`
}

type modelCatalog struct {
	Models        map[string]upstreamModel `json:"models"`
	SearchConfig  []searchModel            `json:"search_config"`
	DefaultModels map[string]string        `json:"default_models"`
}

type catalogCache struct {
	BaseURL   string       `json:"base_url"`
	FetchedAt time.Time    `json:"fetched_at"`
	Catalog   modelCatalog `json:"catalog"`
}

type modelMapping struct {
	ID          string
	Mode        string
	Preference  string
	Label       string
	Description string
}

func shippedModels() []modelMapping {
	return []modelMapping{
		{ID: "pplx-auto", Mode: "concise", Preference: "turbo", Label: "Perplexity Auto"},
		{ID: "pplx-pro", Mode: "copilot", Preference: "pplx_pro", Label: "Perplexity Pro"},
		{ID: "pplx-reasoning", Mode: "copilot", Preference: "pplx_reasoning", Label: "Perplexity Reasoning"},
		{ID: "pplx-deep-research", Mode: "copilot", Preference: "pplx_alpha", Label: "Perplexity Deep Research"},
	}
}

func (rt *pluginRuntime) catalogFor(ctx context.Context, client sender) modelCatalog {
	rt.catalogMu.Lock()
	defer rt.catalogMu.Unlock()
	rt.mu.Lock()
	cache, dir := rt.catalog, rt.authDir
	rt.mu.Unlock()
	if cache != nil && cache.BaseURL == client.cfg.BaseURL && time.Since(cache.FetchedAt) < client.cfg.TTL {
		return cache.Catalog
	}
	if cache = readCatalogFile(dir, client.cfg.BaseURL, client.cfg.TTL); cache == nil {
		var catalog modelCatalog
		if err := client.getJSON(ctx, "/rest/models/config/v2?version=2.18&source=default", nil, &catalog); err != nil || len(catalog.Models) == 0 {
			catalog = modelCatalog{}
		}
		cache = &catalogCache{BaseURL: client.cfg.BaseURL, FetchedAt: time.Now(), Catalog: catalog}
		if len(catalog.Models) > 0 {
			writeCatalogFile(dir, *cache)
		}
	}
	rt.mu.Lock()
	if rt.cfg.BaseURL == client.cfg.BaseURL {
		rt.catalog = cache
	}
	rt.mu.Unlock()
	return cache.Catalog
}

func mappedModels(catalog modelCatalog, tier string, cfg settings) []modelMapping {
	models := shippedModels()
	dynamic := map[string]modelMapping{}
	add := func(internal, label, description string) {
		model, found := catalog.Models[internal]
		if !found || (model.Mode != "search" && model.Mode != "research") {
			return
		}
		if label == "" {
			label = model.Label
		}
		if label == "" {
			label = internal
		}
		if description == "" {
			description = model.Description
		}
		id := "pplx-" + internal
		candidate := modelMapping{ID: id, Mode: "copilot", Preference: internal, Label: label, Description: description}
		if previous, exists := dynamic[id]; !exists || candidate.Label < previous.Label {
			dynamic[id] = candidate
		}
	}
	if len(catalog.SearchConfig) > 0 {
		for _, entry := range catalog.SearchConfig {
			required := ""
			if entry.SubscriptionTier != nil {
				required = *entry.SubscriptionTier
			}
			if required != "" && required != "pro" && required != tier && tier != "max" {
				continue
			}
			add(entry.NonReasoningModel, entry.Label, entry.Description)
			add(entry.ReasoningModel, entry.Label, entry.Description)
		}
	} else {
		for internal := range catalog.Models {
			add(internal, "", "")
		}
	}
	extras := make([]modelMapping, 0, len(dynamic))
	for _, model := range dynamic {
		extras = append(extras, model)
	}
	sort.Slice(extras, func(left, right int) bool {
		if extras[left].Label != extras[right].Label {
			return extras[left].Label < extras[right].Label
		}
		return extras[left].ID < extras[right].ID
	})
	models = append(models, extras...)
	filtered := make([]modelMapping, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		excluded := false
		for _, pattern := range cfg.ExcludeModels {
			if pattern == model.ID || pattern == model.Preference {
				excluded = true
			}
		}
		if !excluded && !seen[model.ID] {
			filtered = append(filtered, model)
			seen[model.ID] = true
		}
	}
	return filtered
}

func modelInfos(mappings []modelMapping) []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(mappings))
	for _, model := range mappings {
		description := model.Description
		if description != "" {
			description += " "
		}
		description += "Text-only Perplexity web chat; image_url parts are ignored."
		models = append(models, pluginapi.ModelInfo{ID: model.ID, Object: "model", OwnedBy: pluginID, DisplayName: model.Label, Name: model.Preference, Description: description, Type: "chat", SupportedGenerationMethods: []string{"chat.completions"}, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}})
	}
	return models
}

func resolveModel(requested string, mappings []modelMapping) (modelMapping, error) {
	requested = strings.TrimPrefix(requested, pluginID+"/")
	for _, model := range mappings {
		if requested == model.ID {
			return model, nil
		}
	}
	internal := strings.TrimPrefix(requested, "pplx-")
	for _, model := range mappings {
		if internal == model.Preference {
			return model, nil
		}
	}
	return modelMapping{}, pluginabi.NewError("unknown_model", "Unknown or excluded Perplexity model", 400)
}

func (rt *pluginRuntime) models(host pluginapi.HostConfigSummary, tier, callbackID string, callback hostCallbackFunc) pluginapi.ModelResponse {
	rt.rememberHost(host)
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(cfg.Timeout)
	defer cancel()
	catalog := rt.catalogFor(ctx, sender{callback: callback, id: callbackID, cfg: cfg})
	return pluginapi.ModelResponse{Provider: pluginID, Models: modelInfos(mappedModels(catalog, tier, cfg))}
}
