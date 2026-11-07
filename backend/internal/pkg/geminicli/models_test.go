package geminicli

import "testing"

func TestDefaultModels_ContainsImageModels(t *testing.T) {
	t.Parallel()

	byID := make(map[string]Model, len(DefaultModels))
	for _, model := range DefaultModels {
		byID[model.ID] = model
	}

	required := []string{
		"gemini-3.1-flash-image",
		"gemini-3-pro-image",
		DefaultTestModel,
	}

	for _, id := range required {
		if _, ok := byID[id]; !ok {
			t.Fatalf("expected curated Gemini model %q to exist", id)
		}
	}
}

func TestGoogleOneModels_CurrentTextCatalog(t *testing.T) {
	t.Parallel()

	mapping := GoogleOneModelMapping()
	for _, id := range []string{"gemini-3.8-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro-preview", "gemini-4-argon"} {
		if mapping[id] != id {
			t.Fatalf("expected Google One model %q to map to itself", id)
		}
	}
	for _, id := range []string{"gemini-2.0-flash", "gemini-2.5-flash", "gemini-2.5-pro", "gemini-3.1-flash-image"} {
		if _, ok := mapping[id]; ok {
			t.Fatalf("did not expect unsupported Google One model %q", id)
		}
	}

	mapping["gemini-3.8-flash"] = "mutated"
	if GoogleOneModelMapping()["gemini-3.8-flash"] != "gemini-3.8-flash" {
		t.Fatal("GoogleOneModelMapping must return a defensive copy")
	}
}
