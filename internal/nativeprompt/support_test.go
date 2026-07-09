package nativeprompt

import (
	"errors"
	"testing"
)

func assertUnsupported(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error %v does not unwrap to ErrUnsupported", err)
	}
}

// TestSupportsAcceptsDynamicTemplate pins that the exact generated prompt with
// proven inputs is claimed.
func TestSupportsAcceptsDynamicTemplate(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strptr("hi")},
		{Role: "user", Parts: []ContentPart{
			{Text: strptr("look")},
			{Image: &Media{Kind: MediaImage, URL: "https://x/y.png"}},
			{OutputFormat: true},
		}},
	}
	if err := Supports(rawDynamicPrompt, msgs); err != nil {
		t.Fatalf("dynamic template with proven inputs should be supported, got: %v", err)
	}
}

// TestSupportsFeatureDeclines pins the feature-based, fail-closed decline for
// each unsupported template construct, each with its specific feature reason.
func TestSupportsFeatureDeclines(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantFeature string
	}{
		{
			name:        "template_string",
			src:         `{% for m in messages %}{{ MyMacro(m) }}{% endfor %} template_string Foo() #"x"#`,
			wantFeature: FeatureTemplateString,
		},
		{
			name:        "macro_block",
			src:         "{% macro greet(x) %}hi {{ x }}{% endmacro %}{{ greet('a') }}",
			wantFeature: FeatureMacro,
		},
		{
			name:        "import_block",
			src:         "{% import 'other.j2' as o %}{{ o.x }}",
			wantFeature: FeatureMacro,
		},
		{
			name:        "callable_output_format",
			src:         `{{ ctx.output_format(render_null_as="null") }}`,
			wantFeature: FeatureCallableOutputFmt,
		},
		{
			name:        "custom_filter_format",
			src:         `{{ value | format(type="yaml") }}`,
			wantFeature: FeatureUnknownFilter,
		},
		{
			name:        "custom_filter_regex_match",
			src:         `{{ x | regex_match("a.*") }}`,
			wantFeature: FeatureUnknownFilter,
		},
		{
			name:        "custom_filter_sum",
			src:         `{{ items | sum }}`,
			wantFeature: FeatureUnknownFilter,
		},
		{
			name:        "python_format_method",
			src:         `{{ "{}".format(x) }}`,
			wantFeature: FeaturePyFormatMethod,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reasons := UnsupportedTemplateFeatures(tc.src)
			if len(reasons) == 0 {
				t.Fatalf("expected a decline for %q", tc.src)
			}
			found := false
			for _, r := range reasons {
				if r.Feature == tc.wantFeature {
					found = true
				}
			}
			if !found {
				t.Fatalf("wanted feature %q among declines %+v", tc.wantFeature, reasons)
			}
			// And Supports must fail closed with ErrUnsupported.
			assertUnsupported(t, Supports(tc.src, nil))
		})
	}
}

// TestSupportsDeclinesUnrecognizedPrompt pins the exact-match catch-all: a
// template that trips no feature scanner but is not the proven dynamic prompt
// (e.g. one using an enum global or class-alias object) still declines.
func TestSupportsDeclinesUnrecognizedPrompt(t *testing.T) {
	// No template_string / macro / callable output_format / unknown filter, but
	// not the dynamic template — e.g. an enum-global comparison.
	src := `{% if output.category == Category.URGENT %}urgent{% endif %}`
	err := Supports(src, nil)
	assertUnsupported(t, err)
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureUnrecognizedPrompt {
		t.Fatalf("want unrecognized_prompt decline, got %v", err)
	}
}

// TestSupportsDeclinesUnprovenMediaKinds pins the input-level decline: audio,
// pdf, and video media parts are beyond the proven corpus and fail closed even
// with the recognized dynamic template.
func TestSupportsDeclinesUnprovenMediaKinds(t *testing.T) {
	kinds := []struct {
		name string
		part ContentPart
	}{
		{"audio", ContentPart{Audio: &Media{Kind: MediaAudio, URL: "u"}}},
		{"pdf", ContentPart{PDF: &Media{Kind: MediaPDF, URL: "u"}}},
		{"video", ContentPart{Video: &Media{Kind: MediaVideo, URL: "u"}}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			msgs := []Message{{Role: "user", Parts: []ContentPart{k.part}}}
			err := Supports(rawDynamicPrompt, msgs)
			assertUnsupported(t, err)
			var d *Decline
			if !errors.As(err, &d) || d.Feature != FeatureUnsupportedMediaKind {
				t.Fatalf("want unsupported_media_kind decline, got %v", err)
			}
		})
	}
}

// TestSupportsDeclinesInvalidMedia pins Fix #3: a media part that violates the
// URL/Base64 one-of invariant (both set, or neither) is declined fail-closed —
// matching bamlutils.MediaInput.Validate — so native never claims parity for a
// media shape BAML rejects. This holds even for the proven image kind.
func TestSupportsDeclinesInvalidMedia(t *testing.T) {
	cases := []struct {
		name  string
		media *Media
	}{
		{"image_both", &Media{Kind: MediaImage, URL: "https://x/y.png", Base64: "AAAA"}},
		{"image_neither", &Media{Kind: MediaImage}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []Message{{Role: "user", Parts: []ContentPart{{Image: tc.media}}}}
			err := Supports(rawDynamicPrompt, msgs)
			assertUnsupported(t, err)
			var d *Decline
			if !errors.As(err, &d) || d.Feature != FeatureInvalidMedia {
				t.Fatalf("want invalid_media decline, got %v", err)
			}
			// Render must also fail closed, not lower the invalid media.
			if _, rerr := Render(msgs, simpleSchema()); !errors.Is(rerr, ErrUnsupported) {
				t.Fatalf("Render should decline invalid media, got %v", rerr)
			}
		})
	}

	// A well-formed image (exactly one of url/base64) is still accepted.
	t.Run("image_valid_url_ok", func(t *testing.T) {
		msgs := []Message{{Role: "user", Parts: []ContentPart{
			{Image: &Media{Kind: MediaImage, URL: "https://x/y.png"}},
		}}}
		if err := Supports(rawDynamicPrompt, msgs); err != nil {
			t.Fatalf("valid image should be supported, got %v", err)
		}
	})
}

// TestDynamicTemplatePassesFeatureScan guards that the proven template itself
// never trips the feature scanner (a false positive there would silently break
// production routing when the predicate is eventually wired in).
func TestDynamicTemplatePassesFeatureScan(t *testing.T) {
	if reasons := UnsupportedTemplateFeatures(rawDynamicPrompt); len(reasons) != 0 {
		t.Fatalf("dynamic template tripped the feature scan: %+v", reasons)
	}
}
