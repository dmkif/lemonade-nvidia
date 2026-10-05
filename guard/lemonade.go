package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
)

type Loaded struct {
	Name string
	Type string
	Busy bool
}

// Lemonade is the subset of Lemonade's admin API the guard uses.
type Lemonade interface {
	Ready(ctx context.Context) bool
	Loaded(ctx context.Context) ([]Loaded, error)
	Load(ctx context.Context, model string) error
	Unload(ctx context.Context, model string) error // "" unloads everything
	Downloaded(ctx context.Context) (map[string]bool, error)
	BackendInstalled(ctx context.Context, recipe, backend string) (bool, error)
	Install(ctx context.Context, recipe, backend string) error
	Pull(ctx context.Context, model string) error
}

// VRAM reports used GPU memory in MiB; an error disables the checks that need it.
type VRAM interface {
	UsedMiB(ctx context.Context) (int, error)
}

type httpLemonade struct {
	base, key string
	c         *http.Client
}

func newHTTPLemonade(base, key string) *httpLemonade {
	return &httpLemonade{base: strings.TrimRight(base, "/"), key: key, c: &http.Client{}}
}

func (l *httpLemonade) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (l *httpLemonade) Ready(ctx context.Context) bool {
	return l.do(ctx, "GET", "/v1/health", nil, nil) == nil
}

func (l *httpLemonade) Loaded(ctx context.Context) ([]Loaded, error) {
	var h struct {
		All []struct {
			Name string `json:"model_name"`
			Type string `json:"type"`
			Busy bool   `json:"is_busy"`
		} `json:"all_models_loaded"`
	}
	if err := l.do(ctx, "GET", "/v1/health", nil, &h); err != nil {
		return nil, err
	}
	var r []Loaded
	for _, m := range h.All {
		r = append(r, Loaded{m.Name, m.Type, m.Busy})
	}
	return r, nil
}

func (l *httpLemonade) Load(ctx context.Context, m string) error {
	return l.do(ctx, "POST", "/v1/load", map[string]string{"model_name": m}, nil)
}

func (l *httpLemonade) Unload(ctx context.Context, m string) error {
	var body any = map[string]string{}
	if m != "" {
		body = map[string]string{"model_name": m}
	}
	err := l.do(ctx, "POST", "/v1/unload", body, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil // already gone
	}
	return err
}

func (l *httpLemonade) Downloaded(ctx context.Context) (map[string]bool, error) {
	var r struct {
		Data []struct {
			ID         string `json:"id"`
			Downloaded bool   `json:"downloaded"`
		} `json:"data"`
	}
	if err := l.do(ctx, "GET", "/v1/models?show_all=true", nil, &r); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, d := range r.Data {
		m[d.ID] = d.Downloaded
	}
	return m, nil
}

func (l *httpLemonade) BackendInstalled(ctx context.Context, recipe, backend string) (bool, error) {
	var r struct {
		Recipes map[string]struct {
			Backends map[string]struct {
				State string `json:"state"`
			} `json:"backends"`
		} `json:"recipes"`
	}
	if err := l.do(ctx, "GET", "/v1/system-info", nil, &r); err != nil {
		return false, err
	}
	return r.Recipes[recipe].Backends[backend].State == "installed", nil
}

func (l *httpLemonade) Install(ctx context.Context, recipe, backend string) error {
	return l.do(ctx, "POST", "/v1/install", map[string]string{"recipe": recipe, "backend": backend}, nil)
}

func (l *httpLemonade) Pull(ctx context.Context, m string) error {
	return l.do(ctx, "POST", "/v1/pull", map[string]any{"model_name": m, "stream": false}, nil)
}

type nvidiaSMI struct{}

func (nvidiaSMI) UsedMiB(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
}
