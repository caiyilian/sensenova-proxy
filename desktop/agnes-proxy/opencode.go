package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const agnesOpenCodeSchema = "https://opencode.ai/config.json"

type agnesOpenCodeLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type agnesOpenCodeModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type agnesOpenCodeThinking struct {
	Type string `json:"type"`
}

type agnesOpenCodeModelOptions struct {
	Thinking agnesOpenCodeThinking `json:"thinking"`
}

type agnesOpenCodeModel struct {
	Name       string                     `json:"name"`
	Modalities *agnesOpenCodeModalities   `json:"modalities,omitempty"`
	Limit      agnesOpenCodeLimit         `json:"limit"`
	Options    *agnesOpenCodeModelOptions `json:"options,omitempty"`
}

type agnesOpenCodeProvider struct {
	NPM     string                        `json:"npm"`
	Name    string                        `json:"name"`
	Options map[string]string             `json:"options"`
	Models  map[string]agnesOpenCodeModel `json:"models"`
}

type agnesOpenCodeExport struct {
	Schema           string                           `json:"$schema"`
	Model            string                           `json:"model"`
	Provider         map[string]agnesOpenCodeProvider `json:"provider"`
	EnabledProviders []string                         `json:"enabled_providers"`
}

type OpenCodeSyncResult struct {
	Created    bool
	Updated    bool
	Unchanged  bool
	BackupPath string
}

func agnesLocalBaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

func defaultOpenCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("读取用户目录：%w", err)
	}
	return filepath.Join(home, ".config", "opencode", "opencode.jsonc"), nil
}

func agnesOpenCodeModels() map[string]agnesOpenCodeModel {
	vision := &agnesOpenCodeModalities{Input: []string{"text", "image"}, Output: []string{"text"}}
	return map[string]agnesOpenCodeModel{
		"agnes-2.0-flash": {
			Name:  "Agnes 2.0 Flash",
			Limit: agnesOpenCodeLimit{Context: 524288, Output: 65536},
		},
		"agnes-2.5-flash": {
			Name:       "Agnes 2.5 Flash",
			Modalities: vision,
			Limit:      agnesOpenCodeLimit{Context: 524288, Output: 65536},
			Options:    &agnesOpenCodeModelOptions{Thinking: agnesOpenCodeThinking{Type: "disabled"}},
		},
		"agnes-3.0-flash": {
			Name:       "Agnes 3.0 Flash",
			Modalities: vision,
			Limit:      agnesOpenCodeLimit{Context: 524288, Output: 65536},
			Options:    &agnesOpenCodeModelOptions{Thinking: agnesOpenCodeThinking{Type: "enabled"}},
		},
	}
}

func newAgnesOpenCodeProvider(baseURL, localToken string) agnesOpenCodeProvider {
	return agnesOpenCodeProvider{
		NPM:  "@ai-sdk/openai-compatible",
		Name: "Agnes Resilient",
		Options: map[string]string{
			"baseURL": baseURL,
			"apiKey":  localToken,
		},
		Models: agnesOpenCodeModels(),
	}
}

func marshalAgnesOpenCodeConfig(baseURL, localToken string) ([]byte, error) {
	config := agnesOpenCodeExport{
		Schema: agnesOpenCodeSchema,
		Model:  "agnes-proxy/agnes-2.0-flash",
		Provider: map[string]agnesOpenCodeProvider{
			"agnes-proxy": newAgnesOpenCodeProvider(baseURL, localToken),
		},
		EnabledProviders: []string{"agnes-proxy"},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 OpenCode 配置：%w", err)
	}
	return append(data, '\n'), nil
}

func writeNewAgnesOpenCodeConfig(path, baseURL, localToken string) error {
	data, err := marshalAgnesOpenCodeConfig(baseURL, localToken)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建 OpenCode 配置目录：%w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入 OpenCode 配置：%w", err)
	}
	return nil
}

func syncAgnesOpenCodeConfig(path, baseURL, localToken string) (OpenCodeSyncResult, error) {
	result := OpenCodeSyncResult{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		result.Created = true
		return result, writeNewAgnesOpenCodeConfig(path, baseURL, localToken)
	}
	if err != nil {
		return result, fmt.Errorf("读取 OpenCode 配置：%w", err)
	}

	updated, err := updateAgnesOpenCodeProvider(data, baseURL, localToken)
	if err != nil {
		return result, err
	}
	if bytes.Equal(data, updated) {
		result.Unchanged = true
		return result, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return result, fmt.Errorf("读取 OpenCode 配置属性：%w", err)
	}
	result.BackupPath = path + ".agnes-proxy.bak"
	if err := os.WriteFile(result.BackupPath, data, info.Mode().Perm()); err != nil {
		return result, fmt.Errorf("备份 OpenCode 配置：%w", err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return result, fmt.Errorf("更新 OpenCode 配置：%w", err)
	}
	result.Updated = true
	return result, nil
}

func updateAgnesOpenCodeProvider(data []byte, baseURL, localToken string) ([]byte, error) {
	rootStart, rootEnd, err := findRootJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("OpenCode 配置不是有效的 JSON/JSONC 对象：%w", err)
	}
	providerStart, providerEnd, providerErr := findNamedJSONObject(data[rootStart:rootEnd], "provider")
	if providerErr != nil {
		providerValue, marshalErr := json.MarshalIndent(map[string]agnesOpenCodeProvider{
			"agnes-proxy": newAgnesOpenCodeProvider(baseURL, localToken),
		}, "", "  ")
		if marshalErr != nil {
			return nil, marshalErr
		}
		data = insertJSONObjectProperty(data, rootStart, rootEnd, "provider", providerValue)
	} else {
		providerStart += rootStart
		providerEnd += rootStart
		agnesStart, agnesEnd, agnesErr := findNamedJSONObject(data[providerStart:providerEnd], "agnes-proxy")
		if agnesErr != nil {
			providerValue, marshalErr := json.MarshalIndent(newAgnesOpenCodeProvider(baseURL, localToken), "", "  ")
			if marshalErr != nil {
				return nil, marshalErr
			}
			data = insertJSONObjectProperty(data, providerStart, providerEnd, "agnes-proxy", providerValue)
		} else {
			agnesStart += providerStart
			agnesEnd += providerStart
			provider := append([]byte(nil), data[agnesStart:agnesEnd]...)
			provider, err = updateExistingAgnesProvider(provider, baseURL, localToken)
			if err != nil {
				return nil, err
			}
			data = replaceJSONRange(data, agnesStart, agnesEnd, provider)
		}
	}
	return ensureEnabledOpenCodeProvider(data, "agnes-proxy")
}

func updateExistingAgnesProvider(provider []byte, baseURL, localToken string) ([]byte, error) {
	optionsStart, optionsEnd, optionsErr := findNamedJSONObject(provider, "options")
	if optionsErr != nil {
		value, err := json.Marshal(map[string]string{"baseURL": baseURL, "apiKey": localToken})
		if err != nil {
			return nil, err
		}
		provider = insertJSONObjectProperty(provider, 0, len(provider), "options", value)
	} else {
		var err error
		provider, err = upsertJSONStringPropertyInObject(provider, optionsStart, optionsEnd, "baseURL", baseURL)
		if err != nil {
			return nil, fmt.Errorf("更新 agnes-proxy options.baseURL：%w", err)
		}
		optionsStart, optionsEnd, err = findNamedJSONObject(provider, "options")
		if err != nil {
			return nil, err
		}
		provider, err = upsertJSONStringPropertyInObject(provider, optionsStart, optionsEnd, "apiKey", localToken)
		if err != nil {
			return nil, fmt.Errorf("更新 agnes-proxy options.apiKey：%w", err)
		}
	}

	modelsStart, modelsEnd, modelsErr := findNamedJSONObject(provider, "models")
	if modelsErr != nil {
		value, err := json.MarshalIndent(agnesOpenCodeModels(), "", "  ")
		if err != nil {
			return nil, err
		}
		return insertJSONObjectProperty(provider, 0, len(provider), "models", value), nil
	}
	for _, modelID := range []string{"agnes-2.0-flash", "agnes-2.5-flash", "agnes-3.0-flash"} {
		modelsStart, modelsEnd, modelsErr = findNamedJSONObject(provider, "models")
		if modelsErr != nil {
			return nil, errors.New("agnes-proxy provider 的 models 字段不是对象")
		}
		_, _, modelErr := findNamedJSONObject(provider[modelsStart:modelsEnd], modelID)
		if modelErr == nil {
			continue
		}
		if hasJSONProperty(provider[modelsStart:modelsEnd], modelID) {
			return nil, fmt.Errorf("agnes-proxy 模型 %s 的配置不是对象", modelID)
		}
		value, err := json.MarshalIndent(agnesOpenCodeModels()[modelID], "", "  ")
		if err != nil {
			return nil, err
		}
		provider = insertJSONObjectProperty(provider, modelsStart, modelsEnd, modelID, value)
	}
	return provider, nil
}

func findRootJSONObject(data []byte) (int, int, error) {
	for index, value := range data {
		if value == '{' {
			end, err := matchingJSONBrace(data, index)
			if err != nil {
				return 0, 0, err
			}
			return index, end + 1, nil
		}
	}
	return 0, 0, errors.New("root object not found")
}

func insertJSONObjectProperty(data []byte, objectStart, objectEnd int, name string, value []byte) []byte {
	newline := "\n"
	if strings.Contains(string(data), "\r\n") {
		newline = "\r\n"
	}
	indent := objectChildIndent(data[objectStart:objectEnd])
	formattedValue := strings.ReplaceAll(string(value), "\n", newline+indent)
	encodedName, _ := json.Marshal(name)
	entry := newline + indent + string(encodedName) + ": " + formattedValue
	if containsJSONValue(data[objectStart+1 : objectEnd-1]) {
		entry += ","
	}
	entry += newline + indent
	result := make([]byte, 0, len(data)+len(entry))
	result = append(result, data[:objectStart+1]...)
	result = append(result, entry...)
	result = append(result, data[objectStart+1:]...)
	return result
}

func objectChildIndent(object []byte) string {
	newline := strings.IndexByte(string(object), '\n')
	if newline >= 0 {
		var indent strings.Builder
		for _, value := range string(object[newline+1:]) {
			if value != ' ' && value != '\t' && value != '\r' {
				break
			}
			if value != '\r' {
				indent.WriteRune(value)
			}
		}
		if indent.Len() > 0 {
			return indent.String()
		}
	}
	return "  "
}

func containsJSONValue(data []byte) bool {
	lineComment := false
	blockComment := false
	for index := 0; index < len(data); index++ {
		current := data[index]
		next := byte(0)
		if index+1 < len(data) {
			next = data[index+1]
		}
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && next == '/' {
				blockComment = false
				index++
			}
			continue
		}
		if current == '/' && next == '/' {
			lineComment = true
			index++
			continue
		}
		if current == '/' && next == '*' {
			blockComment = true
			index++
			continue
		}
		if current != ' ' && current != '\t' && current != '\r' && current != '\n' {
			return true
		}
	}
	return false
}

func ensureEnabledOpenCodeProvider(data []byte, providerName string) ([]byte, error) {
	arrayPattern := regexp.MustCompile(`"enabled_providers"\s*:\s*\[`)
	match := arrayPattern.FindIndex(data)
	if match != nil {
		start := match[1] - 1
		end, err := matchingJSONContainer(data, start, '[', ']')
		if err != nil {
			return nil, fmt.Errorf("读取 enabled_providers：%w", err)
		}
		encoded, _ := json.Marshal(providerName)
		if regexp.MustCompile(`"`+regexp.QuoteMeta(providerName)+`"`).Find(data[start+1:end]) != nil {
			return data, nil
		}
		insertion := append([]byte(nil), encoded...)
		if containsJSONValue(data[start+1 : end]) {
			insertion = append(insertion, ',')
		}
		result := make([]byte, 0, len(data)+len(insertion))
		result = append(result, data[:start+1]...)
		result = append(result, insertion...)
		result = append(result, data[start+1:]...)
		return result, nil
	}

	rootStart, rootEnd, err := findRootJSONObject(data)
	if err != nil {
		return nil, err
	}
	value, _ := json.Marshal([]string{providerName})
	return insertJSONObjectProperty(data, rootStart, rootEnd, "enabled_providers", value), nil
}

func findNamedJSONObject(data []byte, name string) (int, int, error) {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*:\s*\{`)
	match := pattern.FindIndex(data)
	if match == nil {
		return 0, 0, errors.New("object not found")
	}
	openOffset := strings.LastIndex(string(data[match[0]:match[1]]), "{")
	if openOffset < 0 {
		return 0, 0, errors.New("object opening brace not found")
	}
	start := match[0] + openOffset
	end, err := matchingJSONBrace(data, start)
	if err != nil {
		return 0, 0, err
	}
	return start, end + 1, nil
}

func hasJSONProperty(data []byte, name string) bool {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*:`)
	return pattern.FindIndex(data) != nil
}

func matchingJSONBrace(data []byte, start int) (int, error) {
	return matchingJSONContainer(data, start, '{', '}')
}

func matchingJSONContainer(data []byte, start int, opening, closing byte) (int, error) {
	depth := 0
	inString := false
	escaped := false
	lineComment := false
	blockComment := false
	for index := start; index < len(data); index++ {
		current := data[index]
		next := byte(0)
		if index+1 < len(data) {
			next = data[index+1]
		}
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && next == '/' {
				blockComment = false
				index++
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}
		if current == '/' && next == '/' {
			lineComment = true
			index++
			continue
		}
		if current == '/' && next == '*' {
			blockComment = true
			index++
			continue
		}
		switch current {
		case '"':
			inString = true
		case opening:
			depth++
		case closing:
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, errors.New("container closing delimiter not found")
}

func upsertJSONStringPropertyInObject(data []byte, objectStart, objectEnd int, name, value string) ([]byte, error) {
	object := append([]byte(nil), data[objectStart:objectEnd]...)
	updated, err := replaceJSONStringProperty(object, name, value)
	if err != nil {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return nil, marshalErr
		}
		updated = insertJSONObjectProperty(object, 0, len(object), name, encoded)
	}
	return replaceJSONRange(data, objectStart, objectEnd, updated), nil
}

func replaceJSONStringProperty(data []byte, name, value string) ([]byte, error) {
	pattern := regexp.MustCompile(`("` + regexp.QuoteMeta(name) + `"\s*:\s*)"(?:\\.|[^"\\])*"`)
	match := pattern.FindSubmatchIndex(data)
	if match == nil {
		return nil, errors.New("property not found")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(data)+len(encoded))
	result = append(result, data[:match[2]]...)
	result = append(result, data[match[2]:match[3]]...)
	result = append(result, encoded...)
	result = append(result, data[match[1]:]...)
	return result, nil
}

func replaceJSONRange(data []byte, start, end int, replacement []byte) []byte {
	result := make([]byte, 0, len(data)-end+start+len(replacement))
	result = append(result, data[:start]...)
	result = append(result, replacement...)
	result = append(result, data[end:]...)
	return result
}
