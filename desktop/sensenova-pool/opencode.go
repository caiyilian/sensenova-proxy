package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const openCodeSchema = "https://opencode.ai/config.json"

type openCodeLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type openCodeModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type openCodeModel struct {
	Name       string              `json:"name"`
	Modalities *openCodeModalities `json:"modalities,omitempty"`
	Limit      openCodeLimit       `json:"limit"`
}

type openCodeProvider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options map[string]string        `json:"options"`
	Models  map[string]openCodeModel `json:"models"`
}

type openCodeExport struct {
	Schema           string                      `json:"$schema"`
	Model            string                      `json:"model"`
	Provider         map[string]openCodeProvider `json:"provider"`
	EnabledProviders []string                    `json:"enabled_providers"`
}

func gatewayBindHost(allowLAN bool) string {
	if allowLAN {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

func localBaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

func lanBaseURL(port int) (string, error) {
	ip, err := preferredLANIPv4()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d/v1", ip, port), nil
}

func preferredLANIPv4() (string, error) {
	connection, err := net.Dial("udp4", "8.8.8.8:80")
	if err == nil {
		local := connection.LocalAddr()
		_ = connection.Close()
		if udp, ok := local.(*net.UDPAddr); ok && isUsableLANIPv4(udp.IP) {
			return udp.IP.String(), nil
		}
	}

	addresses, listErr := net.InterfaceAddrs()
	if listErr != nil {
		return "", fmt.Errorf("读取本机网络地址：%w", listErr)
	}
	var candidates []string
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if isUsableLANIPv4(ip) {
			candidates = append(candidates, ip.String())
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("没有找到可用的局域网 IPv4 地址")
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

func isUsableLANIPv4(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast()
}

func defaultOpenCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("读取用户目录：%w", err)
	}
	return filepath.Join(home, ".config", "opencode", "opencode.jsonc"), nil
}

func senseNovaOpenCodeProvider(baseURL, localToken string) openCodeProvider {
	vision := &openCodeModalities{Input: []string{"text", "image"}, Output: []string{"text"}}
	return openCodeProvider{
		NPM:  "@ai-sdk/openai-compatible",
		Name: "SenseNova Pool",
		Options: map[string]string{
			"baseURL": baseURL,
			"apiKey":  localToken,
		},
		Models: map[string]openCodeModel{
			"deepseek-v4-flash":        {Name: "DeepSeek V4 Flash", Limit: openCodeLimit{Context: 1048576, Output: 65536}},
			"sensenova-6.7-flash-lite": {Name: "SenseNova 6.7 Flash-Lite", Modalities: vision, Limit: openCodeLimit{Context: 262144, Output: 65536}},
			"glm-5.2":                  {Name: "GLM-5.2", Limit: openCodeLimit{Context: 1048576, Output: 131072}},
			"deepseek-v4-pro":          {Name: "DeepSeek V4 Pro", Limit: openCodeLimit{Context: 1048576, Output: 65536}},
			"kimi-k3":                  {Name: "Kimi K3", Modalities: vision, Limit: openCodeLimit{Context: 1048576, Output: 65536}},
			"sensenova-6.8-flash-lite": {Name: "SenseNova 6.8 Flash-Lite", Modalities: vision, Limit: openCodeLimit{Context: 262144, Output: 65536}},
		},
	}
}

func marshalOpenCodeConfig(baseURL, localToken string) ([]byte, error) {
	config := openCodeExport{
		Schema: openCodeSchema,
		Model:  "sensenova-pool/deepseek-v4-flash",
		Provider: map[string]openCodeProvider{
			"sensenova-pool": senseNovaOpenCodeProvider(baseURL, localToken),
		},
		EnabledProviders: []string{"sensenova-pool"},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 OpenCode 配置：%w", err)
	}
	return append(data, '\n'), nil
}

func writeNewOpenCodeConfig(path, baseURL, localToken string) error {
	data, err := marshalOpenCodeConfig(baseURL, localToken)
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

func syncOpenCodeConfig(path, baseURL, localToken string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", writeNewOpenCodeConfig(path, baseURL, localToken)
	}
	if err != nil {
		return "", fmt.Errorf("读取 OpenCode 配置：%w", err)
	}

	updated, err := updateOpenCodeProvider(data, baseURL, localToken)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("读取 OpenCode 配置属性：%w", err)
	}
	backupPath := path + ".sensenova-pool.bak"
	if err := os.WriteFile(backupPath, data, info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("备份 OpenCode 配置：%w", err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return backupPath, fmt.Errorf("更新 OpenCode 配置：%w", err)
	}
	return backupPath, nil
}

func updateOpenCodeProvider(data []byte, baseURL, localToken string) ([]byte, error) {
	rootStart, rootEnd, err := findRootJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("OpenCode 配置不是有效的 JSON/JSONC 对象：%w", err)
	}
	providerStart, providerEnd, providerErr := findNamedJSONObject(data[rootStart:rootEnd], "provider")
	if providerErr != nil {
		providerValue, marshalErr := json.MarshalIndent(map[string]openCodeProvider{
			"sensenova-pool": senseNovaOpenCodeProvider(baseURL, localToken),
		}, "", "  ")
		if marshalErr != nil {
			return nil, marshalErr
		}
		data = insertJSONObjectProperty(data, rootStart, rootEnd, "provider", providerValue)
	} else {
		providerStart += rootStart
		providerEnd += rootStart
		senseStart, senseEnd, senseErr := findNamedJSONObject(data[providerStart:providerEnd], "sensenova-pool")
		if senseErr != nil {
			providerValue, marshalErr := json.MarshalIndent(senseNovaOpenCodeProvider(baseURL, localToken), "", "  ")
			if marshalErr != nil {
				return nil, marshalErr
			}
			data = insertJSONObjectProperty(data, providerStart, providerEnd, "sensenova-pool", providerValue)
		} else {
			senseStart += providerStart
			senseEnd += providerStart
			provider := append([]byte(nil), data[senseStart:senseEnd]...)
			provider, err = replaceJSONStringProperty(provider, "baseURL", baseURL)
			if err != nil {
				return nil, errors.New("sensenova-pool provider 中没有 options.baseURL")
			}
			provider, err = replaceJSONStringProperty(provider, "apiKey", localToken)
			if err != nil {
				return nil, errors.New("sensenova-pool provider 中没有 options.apiKey")
			}
			result := make([]byte, 0, len(data)-senseEnd+senseStart+len(provider))
			result = append(result, data[:senseStart]...)
			result = append(result, provider...)
			result = append(result, data[senseEnd:]...)
			data = result
		}
	}
	return ensureEnabledOpenCodeProvider(data, "sensenova-pool")
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
