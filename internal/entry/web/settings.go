package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/models"
)

// 网页设置页可调的角色。assist（右栏助手）跟随 writer。
var settingRoles = []string{"architect", "writer", "editor"}

type modelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type providerView struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	BaseURL     string   `json:"base_url"`
	Models      []string `json:"models"`
	HasKey      bool     `json:"has_key"`
	KeyHint     string   `json:"key_hint"`
	RequiresKey bool     `json:"requires_key"`
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.lib.config()
	providers := []providerView{}
	for name, pc := range cfg.Providers {
		v := providerView{Name: name, Type: pc.Type, BaseURL: pc.BaseURL, HasKey: pc.APIKey != "",
			KeyHint: host.MaskAPIKey(pc.APIKey), RequiresKey: pc.RequiresAPIKey(name), Models: []string{}}
		for _, m := range pc.Models {
			v.Models = append(v.Models, m.Name)
		}
		if len(v.Models) == 0 && name == cfg.Provider && cfg.ModelName != "" {
			v.Models = append(v.Models, cfg.ModelName)
		}
		providers = append(providers, v)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })

	roles := map[string]*modelRef{}
	for _, role := range settingRoles {
		if rc, ok := cfg.Roles[role]; ok && rc.Provider != "" && rc.Model != "" {
			roles[role] = &modelRef{Provider: rc.Provider, Model: rc.Model}
		} else {
			roles[role] = nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config_path": bootstrap.EffectiveConfigPath(),
		"default":     modelRef{Provider: cfg.Provider, Model: cfg.ModelName},
		"roles":       roles,
		"providers":   providers,
		"presets":     bootstrap.ProviderPresets(),
		"budget_usd":  cfg.Budget.BookUSD,
		"running":     s.lib.runningCount(),
	})
}

type catalogItem struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Context  int     `json:"context"`
	InputPM  float64 `json:"input_per_m"`
	OutputPM float64 `json:"output_per_m"`
}

// handleCatalog 列出注册表里该 provider 可用的模型（OpenRouter 用 "厂商/ID" 形式），供输入时提示。
func (s *server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	out := []catalogItem{}
	for _, m := range models.DefaultRegistry().List("") {
		id := m.ID
		switch {
		case provider == "openrouter":
			id = m.Provider + "/" + m.ID
		case strings.EqualFold(m.Provider, provider):
		default:
			continue
		}
		out = append(out, catalogItem{ID: id, Name: m.Name, Context: m.ContextWindow,
			InputPM: m.InputCostPer1M, OutputPM: m.OutputCostPer1M})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, out)
}

type providerDraft struct {
	Provider     string   `json:"provider"`
	Type         string   `json:"type"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	APIKeyAction string   `json:"api_key_action"` // keep / replace / clear
	APIKey       string   `json:"api_key"`
	TestModel    string   `json:"test_model,omitempty"`
}

// applyProviderDraft 把草稿合入配置副本，并检查被删掉的模型没有被默认或角色引用。
func applyProviderDraft(cfg bootstrap.Config, d providerDraft) (bootstrap.Config, error) {
	d.Provider = strings.TrimSpace(d.Provider)
	if d.Provider == "" {
		return cfg, fmt.Errorf("Chưa nhập tên nhà cung cấp")
	}
	candidate := bootstrap.CloneConfig(cfg)
	pc := candidate.Providers[d.Provider]
	pc.Type = strings.ToLower(strings.TrimSpace(d.Type))
	pc.BaseURL = strings.TrimSpace(d.BaseURL)

	seen := map[string]bool{}
	var list []bootstrap.ModelConfig
	for _, name := range d.Models {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		// 保留已有条目上的上下文窗口等声明
		entry := bootstrap.ModelConfig{Name: name}
		for _, old := range pc.Models {
			if old.Name == name {
				entry = old
			}
		}
		list = append(list, entry)
	}
	if len(list) == 0 {
		return cfg, fmt.Errorf("Hãy thêm ít nhất một model")
	}
	for _, old := range pc.Models {
		if seen[old.Name] {
			continue
		}
		if refs := modelRefs(candidate, d.Provider, old.Name); len(refs) > 0 {
			return cfg, fmt.Errorf("Model %q đang được dùng cho %s, hãy đổi model đang dùng trước khi xoá", old.Name, strings.Join(refs, ", "))
		}
	}
	pc.Models = list

	switch d.APIKeyAction {
	case "", "keep":
	case "replace":
		pc.APIKey = strings.TrimSpace(d.APIKey)
	case "clear":
		pc.APIKey = ""
	default:
		return cfg, fmt.Errorf("Thao tác API key không hợp lệ: %q", d.APIKeyAction)
	}
	if pc.RequiresAPIKey(d.Provider) && pc.APIKey == "" {
		return cfg, fmt.Errorf("Nhà cung cấp %q cần API key", d.Provider)
	}
	if candidate.Providers == nil {
		candidate.Providers = map[string]bootstrap.ProviderConfig{}
	}
	candidate.Providers[d.Provider] = pc
	return candidate, nil
}

func modelRefs(cfg bootstrap.Config, provider, model string) []string {
	var refs []string
	if cfg.Provider == provider && cfg.ModelName == model {
		refs = append(refs, "mặc định")
	}
	for role, rc := range cfg.Roles {
		if rc.Provider == provider && rc.Model == model {
			refs = append(refs, role)
		}
	}
	sort.Strings(refs)
	return refs
}

func (s *server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	var d providerDraft
	if !readJSON(w, r, &d) {
		return
	}
	candidate, err := applyProviderDraft(s.lib.config(), d)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.applyConfig(candidate); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTestProvider 用草稿建一个临时客户端发最小请求；不保存、不影响正在运行的书。
func (s *server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var d providerDraft
	if !readJSON(w, r, &d) {
		return
	}
	candidate, err := applyProviderDraft(s.lib.config(), d)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	model := strings.TrimSpace(d.TestModel)
	if model == "" && len(d.Models) > 0 {
		model = strings.TrimSpace(d.Models[0])
	}
	candidate.Provider, candidate.ModelName, candidate.Roles = d.Provider, model, nil
	if err := candidate.ValidateBase(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	set, err := bootstrap.NewModelSet(candidate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	msg, err := set.Default.Generate(ctx, []agentcore.Message{agentcore.UserMsg("Reply with the single word OK.")}, nil,
		agentcore.WithMaxTokens(64))
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("Kết nối %s/%s thất bại: %w", d.Provider, model, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "model": model, "reply": clip(msg.Message.TextContent(), 80),
		"ms": time.Since(start).Milliseconds(),
	})
}

func (s *server) handleSaveModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Default   modelRef             `json:"default"`
		Roles     map[string]*modelRef `json:"roles"`
		BudgetUSD *float64             `json:"budget_usd"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	cfg := bootstrap.CloneConfig(s.lib.config())
	if !hasModel(cfg, body.Default) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Model mặc định %s/%s chưa được khai báo", body.Default.Provider, body.Default.Model))
		return
	}
	cfg.Provider, cfg.ModelName = body.Default.Provider, body.Default.Model
	for _, role := range settingRoles {
		ref, ok := body.Roles[role]
		if !ok {
			continue
		}
		if ref == nil || ref.Provider == "" || ref.Model == "" {
			delete(cfg.Roles, role)
			continue
		}
		if !hasModel(cfg, *ref) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("Model %s/%s cho %s chưa được khai báo", ref.Provider, ref.Model, role))
			return
		}
		if cfg.Roles == nil {
			cfg.Roles = map[string]bootstrap.RoleConfig{}
		}
		rc := cfg.Roles[role]
		rc.Provider, rc.Model = ref.Provider, ref.Model
		cfg.Roles[role] = rc
	}
	if body.BudgetUSD != nil {
		if *body.BudgetUSD < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("Giới hạn chi phí không được âm"))
			return
		}
		cfg.Budget.BookUSD = *body.BudgetUSD
	}
	if err := s.lib.applyConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": s.lib.runningCount()})
}

func hasModel(cfg bootstrap.Config, ref modelRef) bool {
	pc, ok := cfg.Providers[ref.Provider]
	if !ok || ref.Model == "" {
		return false
	}
	if len(pc.Models) == 0 {
		return ref.Provider == cfg.Provider && ref.Model == cfg.ModelName
	}
	for _, m := range pc.Models {
		if m.Name == ref.Model {
			return true
		}
	}
	return false
}

// handleOpenRouterCredits 查询 OpenRouter 账户余额与当前 key 的额度，帮用户判断 402 错误的原因。
func (s *server) handleOpenRouterCredits(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.lib.config().Providers["openrouter"]
	if !ok || pc.APIKey == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Chưa cấu hình OpenRouter"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	get := func(path string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/"+path, nil)
		req.Header.Set("Authorization", "Bearer "+pc.APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("OpenRouter trả về %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	var credits struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	var key struct {
		Data struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			Usage          float64  `json:"usage"`
			IsFreeTier     bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := get("credits", &credits); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	_ = get("key", &key)
	writeJSON(w, http.StatusOK, map[string]any{
		"total_credits": credits.Data.TotalCredits,
		"total_usage":   credits.Data.TotalUsage,
		"balance":       credits.Data.TotalCredits - credits.Data.TotalUsage,
		"key_limit":     key.Data.Limit,
		"key_remaining": key.Data.LimitRemaining,
		"key_usage":     key.Data.Usage,
		"is_free_tier":  key.Data.IsFreeTier,
	})
}
