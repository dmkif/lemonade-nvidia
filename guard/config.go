package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a JSON string such as "600s" or "15m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

type ModelCfg struct {
	ResidentMiB int          `json:"resident_mib"` // VRAM while loaded and idle
	PeakMiB     int          `json:"peak_mib"`     // VRAM while a request runs
	Pinned      bool         `json:"pinned"`
	IdleUnload  *Duration    `json:"idle_unload"`
	CPUFallback *CPUFallback `json:"cpu_fallback"`
}

// CPUFallback: while a batch job owns the GPU, small text requests for this model are answered by a
// CPU variant (Lemonade model Alias, llamacpp backend cpu) instead of waiting.
type CPUFallback struct {
	Alias          string `json:"alias"`
	MaxPromptChars int    `json:"max_prompt_chars"`
}

type Config struct {
	Listen        string              `json:"listen"`
	Upstream      string              `json:"upstream"`
	BudgetMiB     int                 `json:"budget_mib"`
	Models        map[string]ModelCfg `json:"models"`
	IdleUnload    Duration            `json:"idle_unload"`
	MaxWait       Duration            `json:"max_wait"`
	Retry5xx      int                 `json:"retry_on_5xx"`
	WarmModel     string              `json:"warm_model"`
	Backends      []string            `json:"backends"`
	SyncModels    []string            `json:"sync_models"`
	RequiredFiles []string            `json:"required_files"` // must exist, else reported missing (no auto-fix)
	LeakMiB       int                 `json:"leak_threshold_mib"`
	LeakAfter     Duration            `json:"leak_after"`
	DrainWait     Duration            `json:"drain_wait"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: ":13305", Upstream: "http://127.0.0.1:13306", BudgetMiB: 11000, Retry5xx: 1, LeakMiB: 1500}
	c.IdleUnload.Duration = 10 * time.Minute
	c.MaxWait.Duration = 15 * time.Minute
	c.LeakAfter.Duration = 60 * time.Second
	c.DrainWait.Duration = 15 * time.Minute
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("guard config: %w", err)
	}
	return c, nil
}

// class of a request for admission control.
type class int

const (
	classLLM class = iota
	classEmbedding
	classBatch
)

func (c class) String() string {
	return [...]string{"llm", "embedding", "batch"}[c]
}

type route struct {
	method string
	path   string
	class  class
	free   bool // no admission, just forward
}

var allowlist = []route{
	{method: "GET", path: "/v1/models", free: true},
	{method: "POST", path: "/v1/chat/completions", class: classLLM},
	{method: "POST", path: "/v1/completions", class: classLLM},
	{method: "POST", path: "/v1/embeddings", class: classEmbedding},
	{method: "POST", path: "/v1/audio/generations", class: classBatch},
	{method: "POST", path: "/v1/images/generations", class: classBatch},
}
