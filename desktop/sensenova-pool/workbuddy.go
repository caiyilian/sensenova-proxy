package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type workBuddyModelSpec struct {
	ID                string
	Name              string
	MaxInputTokens    int
	MaxOutputTokens   int
	SupportsToolCall  bool
	SupportsImages    bool
	SupportsReasoning bool
}

type WorkBuddySyncResult struct {
	Added      int
	Updated    int
	Unchanged  int
	Created    bool
	BackupPath string
}

func defaultWorkBuddyModelsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("读取用户目录：%w", err)
	}
	return filepath.Join(home, ".workbuddy", "models.json"), nil
}

func workBuddyChatURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
}

func senseNovaWorkBuddySpecs() []workBuddyModelSpec {
	return []workBuddyModelSpec{
		{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash · SenseNova", MaxInputTokens: 1048576, MaxOutputTokens: 65536, SupportsToolCall: true, SupportsReasoning: true},
		{ID: "sensenova-6.7-flash-lite", Name: "SenseNova 6.7 Flash-Lite · SenseNova", MaxInputTokens: 262144, MaxOutputTokens: 65536, SupportsToolCall: true, SupportsImages: true, SupportsReasoning: true},
		{ID: "glm-5.2", Name: "GLM-5.2 · SenseNova", MaxInputTokens: 1048576, MaxOutputTokens: 131072, SupportsToolCall: true, SupportsReasoning: true},
		{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro · SenseNova", MaxInputTokens: 1048576, MaxOutputTokens: 65536, SupportsToolCall: true, SupportsReasoning: true},
		{ID: "kimi-k3", Name: "Kimi K3 · SenseNova", MaxInputTokens: 1048576, MaxOutputTokens: 65536, SupportsToolCall: true, SupportsImages: true, SupportsReasoning: true},
		{ID: "sensenova-6.8-flash-lite", Name: "SenseNova 6.8 Flash-Lite · SenseNova", MaxInputTokens: 262144, MaxOutputTokens: 65536, SupportsToolCall: true, SupportsImages: true, SupportsReasoning: true},
	}
}

func syncWorkBuddyConfig(path, endpoint, localToken string) (WorkBuddySyncResult, error) {
	result := WorkBuddySyncResult{}
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("读取 WorkBuddy 配置：%w", err)
	}

	root := make(map[string]json.RawMessage)
	if exists && len(bytes.TrimSpace(data)) > 0 {
		trimmed := bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
		if err := json.Unmarshal(trimmed, &root); err != nil {
			return result, fmt.Errorf("解析 WorkBuddy models.json：%w", err)
		}
	}

	var models []json.RawMessage
	if raw, ok := root["models"]; ok {
		if err := json.Unmarshal(raw, &models); err != nil {
			return result, errors.New("WorkBuddy models.json 的 models 字段不是数组")
		}
	}

	specs := senseNovaWorkBuddySpecs()
	specByID := make(map[string]workBuddyModelSpec, len(specs))
	for _, spec := range specs {
		specByID[spec.ID] = spec
	}
	seen := make(map[string]bool, len(specs))
	conflicts := make([]string, 0)
	updatedModels := make([]json.RawMessage, 0, len(models)+len(specs))
	for _, raw := range models {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return result, fmt.Errorf("WorkBuddy 中存在无法解析的模型项：%w", err)
		}
		id, _ := fields["id"].(string)
		spec, supported := specByID[id]
		if !supported {
			updatedModels = append(updatedModels, raw)
			continue
		}
		if seen[id] {
			return result, fmt.Errorf("WorkBuddy 中存在重复模型 ID：%s", id)
		}
		seen[id] = true
		if !looksLikeSenseNovaWorkBuddyModel(fields) {
			conflicts = append(conflicts, id)
			updatedModels = append(updatedModels, raw)
			continue
		}
		changed := applyWorkBuddySpec(fields, spec, endpoint, localToken)
		if changed {
			encoded, marshalErr := json.Marshal(fields)
			if marshalErr != nil {
				return result, fmt.Errorf("更新 WorkBuddy 模型 %s：%w", id, marshalErr)
			}
			updatedModels = append(updatedModels, encoded)
			result.Updated++
		} else {
			updatedModels = append(updatedModels, raw)
			result.Unchanged++
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return WorkBuddySyncResult{}, fmt.Errorf("以下模型 ID 已被其他来源占用，未修改文件：%s", strings.Join(conflicts, "、"))
	}

	for _, spec := range specs {
		if seen[spec.ID] {
			continue
		}
		fields := make(map[string]any)
		applyWorkBuddySpec(fields, spec, endpoint, localToken)
		encoded, marshalErr := json.Marshal(fields)
		if marshalErr != nil {
			return result, marshalErr
		}
		updatedModels = append(updatedModels, encoded)
		result.Added++
	}
	if result.Added == 0 && result.Updated == 0 {
		return result, nil
	}

	modelsJSON, err := json.Marshal(updatedModels)
	if err != nil {
		return result, fmt.Errorf("生成 WorkBuddy 模型列表：%w", err)
	}
	root["models"] = modelsJSON
	output, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return result, fmt.Errorf("生成 WorkBuddy 配置：%w", err)
	}
	output = append(output, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return result, fmt.Errorf("创建 WorkBuddy 配置目录：%w", err)
	}
	mode := os.FileMode(0o600)
	if exists {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return result, fmt.Errorf("读取 WorkBuddy 配置属性：%w", statErr)
		}
		mode = info.Mode().Perm()
		result.BackupPath = path + ".sensenova-pool.bak"
		if err := os.WriteFile(result.BackupPath, data, mode); err != nil {
			return result, fmt.Errorf("备份 WorkBuddy 配置：%w", err)
		}
	} else {
		result.Created = true
	}
	if err := os.WriteFile(path, output, mode); err != nil {
		return result, fmt.Errorf("写入 WorkBuddy 配置：%w", err)
	}
	return result, nil
}

func looksLikeSenseNovaWorkBuddyModel(fields map[string]any) bool {
	vendor, _ := fields["vendor"].(string)
	name, _ := fields["name"].(string)
	endpoint, _ := fields["url"].(string)
	return strings.EqualFold(strings.TrimSpace(vendor), "SenseNova") ||
		strings.Contains(strings.ToLower(name), "sensenova") ||
		strings.Contains(endpoint, ":18787/")
}

func applyWorkBuddySpec(fields map[string]any, spec workBuddyModelSpec, endpoint, localToken string) bool {
	changed := false
	set := func(name string, value any) {
		if !workBuddyValuesEqual(fields[name], value) {
			fields[name] = value
			changed = true
		}
	}
	set("id", spec.ID)
	set("name", spec.Name)
	set("vendor", "SenseNova")
	set("url", endpoint)
	set("apiKey", localToken)
	set("maxInputTokens", spec.MaxInputTokens)
	set("maxOutputTokens", spec.MaxOutputTokens)
	set("supportsToolCall", spec.SupportsToolCall)
	set("supportsImages", spec.SupportsImages)
	set("supportsReasoning", spec.SupportsReasoning)
	set("useCustomProtocol", false)
	return changed
}

func workBuddyValuesEqual(existing, wanted any) bool {
	switch value := wanted.(type) {
	case int:
		number, ok := existing.(float64)
		if ok {
			return int(number) == value && number == float64(int(number))
		}
		integer, ok := existing.(int)
		return ok && integer == value
	case string:
		text, ok := existing.(string)
		return ok && text == value
	case bool:
		boolean, ok := existing.(bool)
		return ok && boolean == value
	default:
		return false
	}
}
