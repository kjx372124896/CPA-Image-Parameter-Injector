package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
    void* ptr;
    size_t len;
} cliproxy_buffer;

typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    void* call;
    void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

const (
	abiVersion    uint32 = 1
	schemaVersion uint32 = 10 // CLIProxyAPI 8.0.53
	pluginID             = "image-param-injector"
	startMarker          = "[CPA_IMAGE_PARAMS_BEGIN]"
	endMarker            = "[CPA_IMAGE_PARAMS_END]"
)

type pluginConfig struct {
	TargetPaths        []string
	ModelAllowlist     []string
	Position           string
	InjectSize         bool
	InjectN            bool
	InjectAspectRatio  bool
	InjectQuality      bool
	InjectStyle        bool
	InjectOutputFormat bool
	InjectBackground   bool
}

var (
	configMu     sync.RWMutex
	activeConfig = defaultConfig()
)

type lifecycleRequest struct {
	ConfigYAML []byte "json:\"config_yaml\""
}

type requestInterceptRequest struct {
	SourceFormat   string         "json:\"SourceFormat\""
	ToFormat       string         "json:\"ToFormat\""
	Model          string         "json:\"Model\""
	RequestedModel string         "json:\"RequestedModel\""
	Headers        http.Header    "json:\"Headers\""
	Body           []byte         "json:\"Body\""
	Metadata       map[string]any "json:\"Metadata\""
}

type requestInterceptResponse struct {
	Headers http.Header "json:\"Headers,omitempty\""
	Body    []byte      "json:\"Body,omitempty\""
}

type envelope struct {
	OK     bool            "json:\"ok\""
	Result json.RawMessage "json:\"result,omitempty\""
	Error  *envelopeError  "json:\"error,omitempty\""
}

type envelopeError struct {
	Code    string "json:\"code\""
	Message string "json:\"message\""
}

type configField struct {
	Name        string   "json:\"Name\""
	Type        string   "json:\"Type\""
	Default     any      "json:\"Default,omitempty\""
	EnumValues  []string "json:\"EnumValues,omitempty\""
	Description string   "json:\"Description\""
}

type pluginMetadata struct {
	Name             string        "json:\"Name\""
	Version          string        "json:\"Version\""
	Author           string        "json:\"Author\""
	GitHubRepository string        "json:\"GitHubRepository\""
	Logo             string        "json:\"Logo\""
	ConfigFields     []configField "json:\"ConfigFields\""
}

type pluginCapabilities struct {
	RequestInterceptor bool "json:\"request_interceptor\""
}

type registration struct {
	SchemaVersion uint32             "json:\"schema_version\""
	Metadata      pluginMetadata     "json:\"metadata\""
	Capabilities  pluginCapabilities "json:\"capabilities\""
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case "request.intercept_before":
		// The selected upstream format is not known yet. Handle image requests
		// in request.intercept_after, where route and model context are available.
		return okEnvelope(requestInterceptResponse{})
	case "request.intercept_after":
		return interceptAfter(request)
	case "plugin.shutdown":
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		TargetPaths:        []string{"/v1/images/generations", "/v1/images/edits"},
		Position:           "append",
		InjectSize:         true,
		InjectN:            true,
		InjectAspectRatio:  true,
		InjectQuality:      true,
		InjectStyle:        true,
		InjectOutputFormat: true,
		InjectBackground:   true,
	}
}

func currentConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	cfg := activeConfig
	cfg.TargetPaths = append([]string(nil), activeConfig.TargetPaths...)
	cfg.ModelAllowlist = append([]string(nil), activeConfig.ModelAllowlist...)
	return cfg
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("decode plugin lifecycle request: %w", err)
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		parseConfigYAML(string(req.ConfigYAML), &cfg)
	}
	if cfg.Position != "prepend" {
		cfg.Position = "append"
	}
	configMu.Lock()
	activeConfig = cfg
	configMu.Unlock()
	return nil
}

// The plugin only needs a small YAML subset: scalar booleans, position,
// and simple string arrays. This avoids a third-party runtime dependency.
func parseConfigYAML(src string, cfg *pluginConfig) {
	listKey := ""
	targetPathsSet := false
	modelAllowlistSet := false

	for _, sourceLine := range strings.Split(src, "\n") {
		line := strings.TrimSpace(sourceLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "- ") {
			value := parseYAMLScalar(strings.TrimSpace(line[2:]))
			switch listKey {
			case "target_paths":
				cfg.TargetPaths = append(cfg.TargetPaths, value)
			case "model_allowlist":
				cfg.ModelAllowlist = append(cfg.ModelAllowlist, value)
			}
			continue
		}

		listKey = ""
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "target_paths":
			if !targetPathsSet {
				cfg.TargetPaths = nil
				targetPathsSet = true
			}
			if value == "" {
				listKey = key
			} else {
				cfg.TargetPaths = append(cfg.TargetPaths, parseYAMLList(value)...)
			}
		case "model_allowlist":
			if !modelAllowlistSet {
				cfg.ModelAllowlist = nil
				modelAllowlistSet = true
			}
			if value == "" {
				listKey = key
			} else {
				cfg.ModelAllowlist = append(cfg.ModelAllowlist, parseYAMLList(value)...)
			}
		case "position":
			cfg.Position = parseYAMLScalar(value)
		case "inject_size":
			cfg.InjectSize = parseYAMLBool(value, cfg.InjectSize)
		case "inject_n":
			cfg.InjectN = parseYAMLBool(value, cfg.InjectN)
		case "inject_aspect_ratio":
			cfg.InjectAspectRatio = parseYAMLBool(value, cfg.InjectAspectRatio)
		case "inject_quality":
			cfg.InjectQuality = parseYAMLBool(value, cfg.InjectQuality)
		case "inject_style":
			cfg.InjectStyle = parseYAMLBool(value, cfg.InjectStyle)
		case "inject_output_format":
			cfg.InjectOutputFormat = parseYAMLBool(value, cfg.InjectOutputFormat)
		case "inject_background":
			cfg.InjectBackground = parseYAMLBool(value, cfg.InjectBackground)
		}
	}
}

func parseYAMLList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "[]" {
		return nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
		if value == "" {
			return nil
		}
		parts := strings.Split(value, ",")
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if v := parseYAMLScalar(part); v != "" {
				result = append(result, v)
			}
		}
		return result
	}
	if v := parseYAMLScalar(value); v != "" {
		return []string{v}
	}
	return nil
}

func parseYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if decoded, err := strconv.Unquote(value); err == nil {
			return decoded
		}
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

func parseYAMLBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             "CPA Image Parameter Injector",
			Version:          "0.2.0",
			Author:           "kjx372124896",
			GitHubRepository: "https://github.com/kjx372124896/CPA-Image-Parameter-Injector",
			ConfigFields: []configField{
				{Name: "inject_size", Type: "boolean", Default: true, Description: "将 size 转换为画布像素尺寸和比例约束。"},
				{Name: "inject_n", Type: "boolean", Default: true, Description: "将 n 转换为独立图片数量要求，并明确禁止拼图。"},
				{Name: "inject_aspect_ratio", Type: "boolean", Default: true, Description: "将 aspect_ratio / ratio 或 size 推导出的比例写入 Prompt。"},
				{Name: "inject_quality", Type: "boolean", Default: true, Description: "将 quality 写入 Prompt。"},
				{Name: "inject_style", Type: "boolean", Default: true, Description: "将 style 写入 Prompt。"},
				{Name: "inject_output_format", Type: "boolean", Default: true, Description: "将 output_format 作为输出格式偏好写入 Prompt。"},
				{Name: "inject_background", Type: "boolean", Default: true, Description: "将 background 写入 Prompt。"},
				{Name: "position", Type: "enum", Default: "append", EnumValues: []string{"append", "prepend"}, Description: "参数约束追加到原 Prompt 末尾或开头。"},
				{Name: "target_paths", Type: "array", Default: []string{"/v1/images/generations", "/v1/images/edits"}, Description: "允许注入的图片 API 路径。"},
				{Name: "model_allowlist", Type: "array", Default: []string{}, Description: "可选模型白名单；留空表示所有模型。"},
			},
		},
		Capabilities: pluginCapabilities{RequestInterceptor: true},
	}
}

func interceptAfter(raw []byte) ([]byte, error) {
	var req requestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode request interceptor payload: %w", err)
	}
	cfg := currentConfig()
	path, _ := req.Metadata["request_path"].(string)
	if !pathAllowed(path, cfg.TargetPaths) {
		return okEnvelope(requestInterceptResponse{})
	}
	model := req.Model
	if model == "" {
		model = req.RequestedModel
	}
	if !modelAllowed(model, cfg.ModelAllowlist) {
		return okEnvelope(requestInterceptResponse{})
	}

	var updated []byte
	var changed bool
	contentType := req.Headers.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/form-data") {
		updated, changed = injectMultipartImageParams(req.Body, contentType, cfg)
	} else {
		updated, changed = injectImageParams(req.Body, cfg)
	}
	if !changed {
		return okEnvelope(requestInterceptResponse{})
	}
	return okEnvelope(requestInterceptResponse{Body: updated})
}

func pathAllowed(path string, allowlist []string) bool {
	path = normalizePath(path)
	if path == "" {
		return false
	}
	for _, candidate := range allowlist {
		if normalizePath(candidate) == path {
			return true
		}
	}
	return false
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func modelAllowed(model string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, allowed := range allowlist {
		if allowed == "*" || strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

func injectImageParams(body []byte, cfg pluginConfig) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, false
	}
	promptRaw, ok := fields["prompt"]
	if !ok {
		return body, false
	}
	var prompt string
	if err := json.Unmarshal(promptRaw, &prompt); err != nil || strings.TrimSpace(prompt) == "" {
		return body, false
	}

	lines := buildParameterLines(fields, cfg)
	updatedPrompt := composePrompt(prompt, lines, cfg.Position)
	if updatedPrompt == prompt {
		return body, false
	}

	encodedPrompt, err := json.Marshal(updatedPrompt)
	if err != nil {
		return body, false
	}
	fields["prompt"] = encodedPrompt
	updated, err := json.Marshal(fields)
	if err != nil {
		return body, false
	}
	return updated, true
}

const maxMultipartRequestBytes = 128 << 20

type preservedMultipartPart struct {
	header textproto.MIMEHeader
	data   []byte
}

// injectMultipartImageParams updates only the text prompt part. File parts such as
// image and mask are copied byte-for-byte, and the original multipart boundary is
// retained so the existing Content-Type header remains valid.
func injectMultipartImageParams(body []byte, contentType string, cfg pluginConfig) ([]byte, bool) {
	if len(body) == 0 || len(body) > maxMultipartRequestBytes {
		return body, false
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return body, false
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body, false
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	parts := make([]preservedMultipartPart, 0, 8)
	fields := make(map[string]json.RawMessage)
	promptIndex := -1
	for {
		part, errPart := reader.NextRawPart()
		if errPart == io.EOF {
			break
		}
		if errPart != nil {
			return body, false
		}
		data, readErr := io.ReadAll(io.LimitReader(part, maxMultipartRequestBytes+1))
		_ = part.Close()
		if readErr != nil || len(data) > maxMultipartRequestBytes {
			return body, false
		}

		header := make(textproto.MIMEHeader, len(part.Header))
		for key, values := range part.Header {
			header[key] = append([]string(nil), values...)
		}
		name := part.FormName()
		if name != "" && part.FileName() == "" {
			if name == "prompt" {
				if promptIndex >= 0 {
					return body, false
				}
				promptIndex = len(parts)
				fields["prompt"], _ = json.Marshal(string(data))
			} else if _, exists := fields[name]; !exists {
				fields[name], _ = json.Marshal(string(data))
			}
		}
		parts = append(parts, preservedMultipartPart{header: header, data: data})
	}
	if promptIndex < 0 {
		return body, false
	}

	lines := buildParameterLines(fields, cfg)
	updatedPrompt := composePrompt(string(parts[promptIndex].data), lines, cfg.Position)
	if updatedPrompt == string(parts[promptIndex].data) {
		return body, false
	}
	parts[promptIndex].data = []byte(updatedPrompt)

	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(boundary); err != nil {
		return body, false
	}
	for _, item := range parts {
		partWriter, err := writer.CreatePart(item.header)
		if err != nil {
			return body, false
		}
		if _, err := partWriter.Write(item.data); err != nil {
			return body, false
		}
	}
	if err := writer.Close(); err != nil {
		return body, false
	}
	return output.Bytes(), true
}

func composePrompt(prompt string, lines []string, position string) string {
	cleanPrompt := stripInjectedBlock(prompt)
	if len(lines) == 0 {
		return cleanPrompt
	}
	block := startMarker + "\n" + strings.Join(lines, "\n") + "\n" + endMarker
	if position == "prepend" {
		if cleanPrompt == "" {
			return block
		}
		return block + "\n\n" + cleanPrompt
	}
	if cleanPrompt == "" {
		return block
	}
	return cleanPrompt + "\n\n" + block
}

func buildParameterLines(fields map[string]json.RawMessage, cfg pluginConfig) []string {
	lines := make([]string, 0, 8)
	size := jsonString(fields, "size")
	explicitRatio := jsonString(fields, "aspect_ratio")
	if explicitRatio == "" {
		explicitRatio = jsonString(fields, "ratio")
	}
	ratio := explicitRatio
	if ratio == "" {
		ratio = sizeToRatio(size)
	}

	sizeLineAdded := false
	if cfg.InjectSize && size != "" && !strings.EqualFold(size, "auto") {
		displaySize := strings.NewReplacer("x", "×", "X", "×").Replace(size)
		if cfg.InjectAspectRatio && ratio != "" {
			lines = append(lines, fmt.Sprintf("目标画布尺寸：%s 像素；画面宽高比：%s。请尽量严格遵循该画幅，不要擅自改变比例。", displaySize, ratio))
		} else {
			lines = append(lines, fmt.Sprintf("目标画布尺寸：%s 像素。请尽量遵循该画布尺寸。", displaySize))
		}
		sizeLineAdded = true
	}
	if cfg.InjectAspectRatio && ratio != "" && !sizeLineAdded {
		lines = append(lines, fmt.Sprintf("画面宽高比：%s。请保持该比例，不要擅自改为其他比例。", ratio))
	}

	if cfg.InjectN {
		if n, ok := jsonInteger(fields, "n"); ok && n > 0 {
			lines = append(lines, fmt.Sprintf("输出数量：%d 张彼此独立的图片，每张单独生成并单独返回；禁止将多张图片拼成一张网格图、九宫格、拼贴图或联系表。", n))
		}
	}
	if cfg.InjectQuality {
		if quality := jsonString(fields, "quality"); quality != "" && !strings.EqualFold(quality, "auto") {
			lines = append(lines, fmt.Sprintf("画质等级：%s；请尽可能保证细节清晰、纹理完整、边缘干净。", quality))
		}
	}
	if cfg.InjectStyle {
		if style := jsonString(fields, "style"); style != "" {
			lines = append(lines, fmt.Sprintf("风格偏好：%s。", style))
		}
	}
	if cfg.InjectOutputFormat {
		if format := jsonString(fields, "output_format"); format != "" {
			lines = append(lines, fmt.Sprintf("输出文件格式偏好：%s；如果上游支持该编码格式，请按此格式返回，不要把格式要求绘制成画面内容。", strings.ToUpper(format)))
		}
	}
	if cfg.InjectBackground {
		if background := jsonString(fields, "background"); background != "" && !strings.EqualFold(background, "auto") {
			switch strings.ToLower(background) {
			case "transparent":
				lines = append(lines, "背景要求：透明背景；不要用棋盘格图案模拟透明。")
			case "opaque":
				lines = append(lines, "背景要求：不透明背景。")
			default:
				lines = append(lines, fmt.Sprintf("背景模式：%s。", background))
			}
		}
	}
	return lines
}

func stripInjectedBlock(prompt string) string {
	for {
		start := strings.Index(prompt, startMarker)
		if start < 0 {
			return prompt
		}
		endRelative := strings.Index(prompt[start+len(startMarker):], endMarker)
		if endRelative < 0 {
			prompt = strings.TrimRight(prompt[:start], "\n\r\t ")
			return prompt
		}
		end := start + len(startMarker) + endRelative + len(endMarker)
		removeStart := start
		if removeStart >= 2 && prompt[removeStart-2:removeStart] == "\n\n" {
			removeStart -= 2
		}
		if end+2 <= len(prompt) && prompt[end:end+2] == "\n\n" {
			end += 2
		}
		prompt = prompt[:removeStart] + prompt[end:]
	}
}

func jsonString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func jsonInteger(fields map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, false
	}
	var numeric string
	switch typed := value.(type) {
	case json.Number:
		numeric = typed.String()
	case string: // multipart form fields encode numeric values as text.
		numeric = strings.TrimSpace(typed)
	default:
		return 0, false
	}
	parsed, err := strconv.ParseFloat(numeric, 64)
	if err != nil || math.Trunc(parsed) != parsed || parsed > float64(math.MaxInt) || parsed < 0 {
		return 0, false
	}
	return int(parsed), true
}

func sizeToRatio(size string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(size), func(r rune) bool {
		return r == 'x' || r == 'X' || r == '×'
	})
	if len(parts) != 2 {
		return ""
	}
	width, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return ""
	}
	divisor := gcd(width, height)
	return fmt.Sprintf("%d:%d", width/divisor, height/divisor)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
