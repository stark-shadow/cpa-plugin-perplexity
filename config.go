package main

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"gopkg.in/yaml.v3"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"

type settings struct {
	Language           string        `yaml:"language"`
	Sources            []string      `yaml:"sources"`
	IncludeSources     bool          `yaml:"include_sources"`
	CatalogTTL         string        `yaml:"catalog_ttl"`
	RequestTimeout     string        `yaml:"request_timeout"`
	MaxPromptChars     int           `yaml:"max_prompt_chars"`
	MaxConcurrency     int           `yaml:"max_concurrency"`
	ProQueryLimit      int           `yaml:"pro_query_limit"`
	ResearchQueryLimit int           `yaml:"research_query_limit"`
	ExcludeModels      []string      `yaml:"exclude_models"`
	BaseURL            string        `yaml:"base_url"`
	UserAgent          string        `yaml:"user_agent"`
	TTL                time.Duration `yaml:"-"`
	Timeout            time.Duration `yaml:"-"`
}

type rawSettings struct {
	Settings settings `yaml:",inline"`
	Enabled  any      `yaml:"enabled"`
	Priority any      `yaml:"priority"`
	Store    any      `yaml:"store"`
}

func defaultSettings() settings {
	return settings{Language: "en-US", Sources: []string{"web"}, IncludeSources: true, CatalogTTL: "6h", RequestTimeout: "180s", MaxPromptChars: 48000, MaxConcurrency: 2, BaseURL: "https://www.perplexity.ai", UserAgent: browserUA, TTL: 6 * time.Hour, Timeout: 180 * time.Second}
}

func parseSettings(raw []byte) (settings, error) {
	parsed := rawSettings{Settings: defaultSettings()}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil && err != io.EOF {
		return settings{}, pluginabi.NewError("invalid_config", "Invalid Perplexity configuration: check field names and value types", 400)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return settings{}, pluginabi.NewError("invalid_config", "Perplexity configuration must contain exactly one YAML document", 400)
	}
	cfg := parsed.Settings
	invalid := func() (settings, error) {
		return settings{}, pluginabi.NewError("invalid_config", "Invalid Perplexity configuration: check durations, limits, sources, URL and headers", 400)
	}
	var err error
	cfg.TTL, err = time.ParseDuration(cfg.CatalogTTL)
	if err != nil || cfg.TTL <= 0 {
		return invalid()
	}
	cfg.Timeout, err = time.ParseDuration(cfg.RequestTimeout)
	if err != nil || cfg.Timeout <= 0 || cfg.MaxPromptChars < len(truncationMarker) || cfg.MaxConcurrency < 1 || cfg.ProQueryLimit < 0 || cfg.ResearchQueryLimit < 0 {
		return invalid()
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return invalid()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Language == "" || cfg.UserAgent == "" || strings.ContainsAny(cfg.Language+cfg.UserAgent, "\r\n\x00") || len(cfg.Sources) == 0 {
		return invalid()
	}
	for _, source := range cfg.Sources {
		if source != "web" && source != "scholar" && source != "social" {
			return invalid()
		}
	}
	return cfg, nil
}
