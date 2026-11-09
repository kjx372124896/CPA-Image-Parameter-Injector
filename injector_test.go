package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

func TestPluginRegistrationHasRequiredMetadata(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != 10 {
		t.Fatalf("schema version = %d, want 10 for CLIProxyAPI 8.0.53", registration.SchemaVersion)
	}
	if registration.Metadata.Name == "" || registration.Metadata.Version == "" ||
		registration.Metadata.Author == "" || registration.Metadata.GitHubRepository == "" {
		t.Fatalf("plugin metadata is incomplete: %#v", registration.Metadata)
	}
	if !registration.Capabilities.RequestInterceptor {
		t.Fatal("request_interceptor capability was not declared")
	}
}

func TestInterceptAfterRPCEnvelope(t *testing.T) {
	body := []byte("{\"prompt\":\"A product photo\",\"size\":\"1024x1536\",\"n\":3}")
	request := requestInterceptRequest{
		SourceFormat:   "openai",
		ToFormat:       "codex",
		Model:          "gpt-image-2.5",
		RequestedModel: "gpt-image-2.5",
		Body:           body,
		Metadata:       map[string]any{"request_path": "/v1/images/generations"},
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := interceptAfter(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Body []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawResponse, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Result.Body) == 0 {
		t.Fatalf("interceptor did not return a replacement body: %s", rawResponse)
	}
	var updated map[string]json.RawMessage
	if err := json.Unmarshal(response.Result.Body, &updated); err != nil {
		t.Fatal(err)
	}
	var prompt string
	_ = json.Unmarshal(updated["prompt"], &prompt)
	if !strings.Contains(prompt, "1024×1536") || !strings.Contains(prompt, "3 张彼此独立") {
		t.Fatalf("RPC response body was not injected: %s", prompt)
	}
}

func TestInjectImageParams(t *testing.T) {
	input := []byte("{\"model\":\"gpt-image-2.5\",\"prompt\":\"厨房收纳架产品图\",\"size\":\"1024x1536\",\"n\":4,\"quality\":\"high\",\"output_format\":\"png\",\"background\":\"transparent\",\"metadata\":{\"keep\":true}}")
	updated, changed := injectImageParams(input, defaultConfig())
	if !changed {
		t.Fatal("expected request to be modified")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(updated, &body); err != nil {
		t.Fatalf("updated body is invalid JSON: %v", err)
	}
	var prompt string
	if err := json.Unmarshal(body["prompt"], &prompt); err != nil {
		t.Fatalf("prompt is not a string: %v", err)
	}
	for _, expected := range []string{"1024×1536", "2:3", "4 张彼此独立", "high", "PNG", "透明背景"} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("prompt does not contain %q: %s", expected, prompt)
		}
	}
	if !strings.Contains(prompt, "厨房收纳架产品图") {
		t.Fatal("original prompt was lost")
	}
	if _, ok := body["metadata"]; !ok {
		t.Fatal("unrelated request fields were lost")
	}
}

func TestInjectionIsIdempotent(t *testing.T) {
	cfg := defaultConfig()
	input := []byte("{\"prompt\":\"A product photo\",\"size\":\"1024x1024\",\"n\":2}")
	once, changed := injectImageParams(input, cfg)
	if !changed {
		t.Fatal("first injection did not modify the request")
	}
	twice, changed := injectImageParams(once, cfg)
	if changed {
		t.Fatal("second injection should be a no-op when the parameter block is already current")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(twice, &body); err != nil {
		t.Fatal(err)
	}
	var prompt string
	_ = json.Unmarshal(body["prompt"], &prompt)
	if strings.Count(prompt, startMarker) != 1 || strings.Count(prompt, endMarker) != 1 {
		t.Fatalf("injected block accumulated: %s", prompt)
	}
	if strings.Count(prompt, "输出数量：2") != 1 {
		t.Fatalf("n instruction duplicated: %s", prompt)
	}
}

func TestNonImagePathIsRejected(t *testing.T) {
	paths := defaultConfig().TargetPaths
	if pathAllowed("/v1/chat/completions", paths) {
		t.Fatal("chat completions must not be treated as an image endpoint")
	}
	if !pathAllowed("/v1/images/generations", paths) {
		t.Fatal("image generation endpoint should be allowed")
	}
	if !pathAllowed("/v1/images/edits?foo=bar", paths) {
		t.Fatal("query string should not prevent route matching")
	}
	if pathAllowed("/v1/images/edit", paths) {
		t.Fatal("singular image edit typo should not be included in the published route list")
	}
}

type multipartTestFile struct {
	filename    string
	contentType string
	data        []byte
}

func makeMultipartEditBody(t *testing.T) ([]byte, string, []byte, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"model":   "gpt-image-2.5",
		"prompt":  "Retouch this product photo",
		"size":    "1024x1536",
		"n":       "3",
		"quality": "high",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	imageBytes := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xfe, 0xff, '\r', '\n', 0x00}
	imageHeader := make(textproto.MIMEHeader)
	imageHeader.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "image", "filename": "reference.png"}))
	imageHeader.Set("Content-Type", "image/png")
	imagePart, err := writer.CreatePart(imageHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imagePart.Write(imageBytes); err != nil {
		t.Fatal(err)
	}
	maskBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01}
	maskPart, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maskPart.Write(maskBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType(), imageBytes, maskBytes
}

func inspectMultipartEditBody(t *testing.T, body []byte, contentType string) (map[string]string, map[string]multipartTestFile) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := make(map[string]string)
	files := make(map[string]multipartTestFile)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			t.Fatal(err)
		}
		if part.FileName() != "" {
			files[part.FormName()] = multipartTestFile{filename: part.FileName(), contentType: part.Header.Get("Content-Type"), data: data}
		} else {
			fields[part.FormName()] = string(data)
		}
	}
	return fields, files
}

func TestMultipartImageEditInjectionPreservesFilesAndFields(t *testing.T) {
	body, contentType, originalImage, originalMask := makeMultipartEditBody(t)
	updated, changed := injectMultipartImageParams(body, contentType, defaultConfig())
	if !changed {
		t.Fatal("expected multipart image edit body to be modified")
	}
	fields, files := inspectMultipartEditBody(t, updated, contentType)
	for _, expected := range []string{"1024×1536", "2:3", "3 张彼此独立", "画质等级：high"} {
		if !strings.Contains(fields["prompt"], expected) {
			t.Errorf("multipart prompt does not contain %q: %s", expected, fields["prompt"])
		}
	}
	if !strings.Contains(fields["prompt"], "Retouch this product photo") {
		t.Fatal("original multipart prompt was lost")
	}
	if fields["size"] != "1024x1536" || fields["n"] != "3" {
		t.Fatalf("original multipart parameters changed: size=%q n=%q", fields["size"], fields["n"])
	}
	if files["image"].filename != "reference.png" || files["image"].contentType != "image/png" {
		t.Fatalf("image metadata changed: %#v", files["image"])
	}
	if !bytes.Equal(files["image"].data, originalImage) {
		t.Fatalf("image bytes changed: got %v want %v", files["image"].data, originalImage)
	}
	if files["mask"].filename != "mask.png" || !bytes.Equal(files["mask"].data, originalMask) {
		t.Fatalf("mask file changed: %#v", files["mask"])
	}
}

func TestInterceptAfterMultipartEditsEndpoint(t *testing.T) {
	body, contentType, originalImage, originalMask := makeMultipartEditBody(t)
	request := requestInterceptRequest{
		SourceFormat:   "openai",
		ToFormat:       "codex",
		Model:          "gpt-image-2.5",
		RequestedModel: "gpt-image-2.5",
		Headers:        http.Header{"Content-Type": []string{contentType}},
		Body:           body,
		Metadata:       map[string]any{"request_path": "/v1/images/edits"},
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := interceptAfter(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Body []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawResponse, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Result.Body) == 0 {
		t.Fatalf("multipart edit interceptor did not return a replacement body: %s", rawResponse)
	}
	fields, files := inspectMultipartEditBody(t, response.Result.Body, contentType)
	if !strings.Contains(fields["prompt"], "3 张彼此独立") {
		t.Fatalf("multipart edit prompt was not injected: %s", fields["prompt"])
	}
	if !bytes.Equal(files["image"].data, originalImage) || !bytes.Equal(files["mask"].data, originalMask) {
		t.Fatal("image or mask bytes changed in request interceptor RPC path")
	}
}

func TestMultipartInjectionIsIdempotent(t *testing.T) {
	body, contentType, _, _ := makeMultipartEditBody(t)
	once, changed := injectMultipartImageParams(body, contentType, defaultConfig())
	if !changed {
		t.Fatal("first multipart injection did not modify the request")
	}
	twice, changed := injectMultipartImageParams(once, contentType, defaultConfig())
	if changed {
		t.Fatal("second multipart injection should be a no-op when the parameter block is already current")
	}
	fields, _ := inspectMultipartEditBody(t, twice, contentType)
	if strings.Count(fields["prompt"], startMarker) != 1 || strings.Count(fields["prompt"], endMarker) != 1 {
		t.Fatalf("multipart injected block accumulated: %s", fields["prompt"])
	}
}

func TestModelAllowlist(t *testing.T) {
	if !modelAllowed("gpt-image-2.5", []string{"gpt-image-2.5"}) {
		t.Fatal("listed model should be allowed")
	}
	if modelAllowed("gpt-image-1", []string{"gpt-image-2.5"}) {
		t.Fatal("unlisted model should be rejected")
	}
	if !modelAllowed("anything", nil) {
		t.Fatal("empty allowlist should allow all models")
	}
}

func TestSizeToRatio(t *testing.T) {
	cases := map[string]string{
		"1024x1536": "2:3",
		"1536x1024": "3:2",
		"1024×1024": "1:1",
		"auto":      "",
	}
	for size, want := range cases {
		if got := sizeToRatio(size); got != want {
			t.Errorf("sizeToRatio(%q) = %q, want %q", size, got, want)
		}
	}
}

func TestConfigParsing(t *testing.T) {
	cfg := defaultConfig()
	parseConfigYAML("target_paths:\n  - \"/v1/images/generations\"\ninject_size: false\ninject_n: true\nposition: \"prepend\"\nmodel_allowlist:\n  - \"gpt-image-2.5\"\n", &cfg)
	if cfg.InjectSize {
		t.Fatal("inject_size=false was not applied")
	}
	if !cfg.InjectN {
		t.Fatal("inject_n=true was not applied")
	}
	if cfg.Position != "prepend" {
		t.Fatalf("position = %q, want prepend", cfg.Position)
	}
	if len(cfg.TargetPaths) != 1 || cfg.TargetPaths[0] != "/v1/images/generations" {
		t.Fatalf("unexpected target paths: %#v", cfg.TargetPaths)
	}
	if len(cfg.ModelAllowlist) != 1 || cfg.ModelAllowlist[0] != "gpt-image-2.5" {
		t.Fatalf("unexpected model allowlist: %#v", cfg.ModelAllowlist)
	}
}

func TestExistingBlockIsRemovedWhenNoNewParams(t *testing.T) {
	input := []byte("{\"prompt\":\"Original\\n\\n[CPA_IMAGE_PARAMS_BEGIN]\\nold parameters\\n[CPA_IMAGE_PARAMS_END]\"}")
	cfg := defaultConfig()
	cfg.InjectSize = false
	cfg.InjectN = false
	cfg.InjectAspectRatio = false
	cfg.InjectQuality = false
	cfg.InjectStyle = false
	cfg.InjectOutputFormat = false
	cfg.InjectBackground = false
	updated, changed := injectImageParams(input, cfg)
	if !changed {
		t.Fatal("stale marker block should be removed")
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(updated, &body)
	var prompt string
	_ = json.Unmarshal(body["prompt"], &prompt)
	if strings.Contains(prompt, startMarker) || prompt != "Original" {
		t.Fatalf("stale block not removed cleanly: %q", prompt)
	}
}
