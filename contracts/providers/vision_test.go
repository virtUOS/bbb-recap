package providers

// Vision calls and profiles 1.1.0 (providers.md §3.6, §4.1; ADR-017). These
// tests were written before the implementation and fail until it exists
// (README.md "Not implemented yet").

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// slideSchema is a stand-in for slide-description.v1 (templates.md §3.2).
const slideSchema = `{"type": "object", "additionalProperties": false, "required": ["text", "visual"],
  "properties": {"text": {"type": "string", "maxLength": 2000}, "visual": {"type": "string", "maxLength": 1000}}}`

const slideReply = `{"text": "Fixture Chart Title", "visual": "A bar chart."}`

// fixturePNG is a small synthetic PNG.
func fixturePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range 16 {
		img.Set(i%4, i/4, color.RGBA{0, 0, 255, 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// visionParts is the user content of a describe call (providers.md §3.6).
func visionParts(url string) []any {
	return []any{
		map[string]any{"type": "text", "text": "Describe the slide in this image."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
	}
}

// visionInput is the stdin of `recap tool llm complete` with content parts.
func visionInput(parts []any) map[string]any {
	return map[string]any{
		"profile": "vllm-lab",
		"request": map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": systemText},
				map[string]any{"role": "user", "content": parts},
			},
			"output":            map[string]any{"type": "json", "schema": json.RawMessage(slideSchema)},
			"max_output_tokens": 700,
			"call_ref":          jobID + ":describe:slide:1",
		},
	}
}

func TestVision(t *testing.T) {
	t.Run("CT-PROV-13 vision content parts", func(t *testing.T) { testVisionParts(t, harness.Bin(t)) })
	t.Run("CT-PROV-14 profiles 1.1.0", func(t *testing.T) { testVisionProfiles(t, harness.Bin(t)) })
}

// §3.6: the parts go out unchanged as the user content; the schema goes per
// mode; only data:image/png;base64 URLs; 400 on an image call is
// input_too_large.
func testVisionParts(t *testing.T, bin string) {
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG(t))
	for _, mode := range []string{"json_object", "json_schema"} {
		t.Run("parts-"+mode, func(t *testing.T) {
			fake := startFake(t, completion(t, slideReply, "stop"))
			p := loadProfiles(t).set(t, "base_url", llmProfile, fake.url()+"/v1").set(t, "structured_output", 1, mode)
			parts := visionParts(url)
			var out llmOutput
			runJSON(t, bin, toolEnv{providersFile: p.write(t), certFile: fake.certFile(t)}, visionInput(parts), &out, "llm", "complete")
			if out.Response == nil {
				t.Fatalf("error %+v, want a response (content parts not implemented?)", out.Error)
			}
			expectJSON(t, out, slideReply)
			ex := expectCalls(t, fake, 1)[0]
			var body struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
				ResponseFormat map[string]any `json:"response_format"`
			}
			if err := json.Unmarshal(ex.body, &body); err != nil || len(body.Messages) != 2 {
				t.Fatalf("request messages: %v (%d)", err, len(body.Messages))
			}
			want, _ := json.Marshal(parts)
			if !jsonEqualRaw(body.Messages[1].Content, want) {
				t.Errorf("user content = %s, want the parts unchanged %s", body.Messages[1].Content, want)
			}
			var sys string
			if err := json.Unmarshal(body.Messages[0].Content, &sys); err != nil {
				t.Fatalf("system content is not a string: %s", body.Messages[0].Content)
			}
			switch mode {
			case "json_object":
				if !containsJSON(sys, json.RawMessage(slideSchema)) {
					t.Errorf("json_object: system message does not carry the schema (§3.3)")
				}
			case "json_schema":
				js, _ := body.ResponseFormat["json_schema"].(map[string]any)
				got, _ := json.Marshal(js["schema"])
				if !jsonEqualRaw(got, []byte(slideSchema)) {
					t.Errorf("json_schema: response_format schema = %s", got)
				}
			}
		})
	}
	for _, tc := range []struct{ name, url string }{
		{"remote-url", "https://images.recap.test/slide.png"},
		{"http-url", "http://images.recap.test/slide.png"},
		{"jpeg-data-url", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(fixturePNG(t))},
	} {
		t.Run("refused-"+tc.name, func(t *testing.T) {
			fake := startFake(t, completion(t, slideReply, "stop"))
			p := loadProfiles(t).set(t, "base_url", llmProfile, fake.url()+"/v1")
			stdin, _ := json.Marshal(visionInput(visionParts(tc.url)))
			stdout, stderr, code := execTool(t, bin, toolEnv{providersFile: p.write(t), certFile: fake.certFile(t)}, stdin, "llm", "complete")
			if code != 2 {
				t.Errorf("exit %d, want 2 (only data:image/png;base64 URLs, §3.6)\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			expectCalls(t, fake, 0)
		})
	}
	t.Run("400-input_too_large", func(t *testing.T) {
		fake := startFake(t, reply{status: http.StatusBadRequest, body: []byte(`{"error": {"message": "` + canaryLLMError + `"}}`)})
		p := loadProfiles(t).set(t, "base_url", llmProfile, fake.url()+"/v1")
		var out llmOutput
		stdout, stderr := runJSON(t, bin, toolEnv{providersFile: p.write(t), certFile: fake.certFile(t)}, visionInput(visionParts(url)), &out, "llm", "complete")
		expectClass(t, out, "input_too_large")
		expectNotLeaked(t, stdout, stderr, canaryLLMError)
	})
}

// §4.1: defaults.vision names an LLM profile whose declaration (when present)
// lists slide_images; anything else stops the start naming the key.
func testVisionProfiles(t *testing.T, bin string) {
	v := loadProfiles(t).set(t, "schema_version", 1, `"1.1.0"`).with(t, "llm", 1, "vision: vllm-lab").
		set(t, "data_categories", llmProfile, "[transcript, chat, summary, slide_images]")
	t.Run("valid", func(t *testing.T) { expectLoads(t, bin, v) })
	t.Run("valid-in-1.0.0-file", func(t *testing.T) { expectLoads(t, bin, v.set(t, "schema_version", 1, `"1.0.0"`)) })
	t.Run("declaration-absent", func(t *testing.T) {
		expectLoadsWithWarn(t, bin, v.without(t, "declaration", llmProfile), "vllm-lab")
	})
	t.Run("unknown-profile", func(t *testing.T) {
		expectStartupError(t, bin, v.set(t, "vision", 1, "no-such-profile"), "vision")
	})
	t.Run("asr-profile", func(t *testing.T) {
		expectStartupError(t, bin, v.set(t, "vision", 1, "whisperx-lab"), "vision")
	})
	t.Run("declaration-without-slide_images", func(t *testing.T) {
		expectStartupError(t, bin, v.set(t, "data_categories", llmProfile, "[transcript, chat, summary]"), "slide_images")
	})
}
