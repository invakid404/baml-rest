// Test functions for integration testing

// Simple string output
function GetGreeting(name: string) -> string {
    client TestClient
    prompt #"Say hello to {{ name }}"#
}

// Simple object output
function GetSimple(input: string) -> SimpleOutput {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Process: {{ input }}"#
}

// Complex object
function GetPerson(description: string) -> Person {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Extract person from: {{ description }}"#
}

// Nested object
function GetPersonWithAddress(description: string) -> PersonWithAddress {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Extract: {{ description }}"#
}

// Array output
function GetPeople(descriptions: string) -> Person[] {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Extract all people from: {{ descriptions }}"#
}

// Enum output
function GetCategory(item: string) -> CategorizedItem {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Categorize: {{ item }}"#
}

// Dynamic output (for type_builder testing)
function GetDynamic(input: string) -> DynamicOutput {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Process: {{ input }}"#
}

// Dynamic enum output (for type_builder testing)
function GetDynamicEnum(input: string) -> DynamicEnumOutput {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Categorize: {{ input }}"#
}

// Comprehensive output for OpenAPI schema validation
function GetComprehensive(input: string) -> ComprehensiveOutput {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Analyze: {{ input }}"#
}

// Media input: single image from URL or base64
function DescribeImage(img: image) -> string {
    client TestClient
    prompt #"Describe the image: {{ img }}"#
}

// Media input: image alongside a text parameter
function DescribeImageWithPrompt(img: image, prompt: string) -> string {
    client TestClient
    prompt #"{{ prompt }}: {{ img }}"#
}

// Media input: audio type
function TranscribeAudio(audio: audio) -> string {
    client TestClient
    prompt #"Transcribe: {{ audio }}"#
}

// Nested media: class with image field
function DescribeImageWithCaption(input: ImageWithCaption) -> string {
    client TestClient
    prompt #"Describe the image ({{ input.caption }}): {{ input.img }}"#
}

// Nested media: class with optional image + required pdf
function ProcessDocument(bundle: DocumentBundle) -> string {
    client TestClient
    prompt #"Process document '{{ bundle.title }}': {{ bundle.document }}"#
}

// Edge case: direct param as list of media (image[])
function DescribeImages(images: image[]) -> string {
    client TestClient
    prompt #"Describe all images: {{ images }}"#
}

// Edge case: direct param as optional media (image?)
function DescribeOptionalImage(img: image?) -> string {
    client TestClient
    prompt #"Describe if present: {{ img }}"#
}

// Edge case: direct param as list of optional media ((image | null)[])
function DescribeOptionalImages(images: (image | null)[]) -> string {
    client TestClient
    prompt #"Describe optional images: {{ images }}"#
}

// Edge case: nested class with list of media (image[])
function DescribeGallery(gallery: ImageGallery) -> string {
    client TestClient
    prompt #"Describe gallery '{{ gallery.title }}': {{ gallery.images }}"#
}

// Edge case: nested class with list of optional media ((image | null)[])
function DescribeOptionalGallery(gallery: OptionalImageGallery) -> string {
    client TestClient
    prompt #"Describe optional gallery '{{ gallery.title }}': {{ gallery.images }}"#
}

// Edge case P2: optional list of media (image[]? -> *[]Image)
function DescribeOptionalImageList(images: image[]?) -> string {
    client TestClient
    prompt #"Describe optional image list: {{ images }}"#
}

// Edge case P1: optional class param with media (ImageWithCaption? -> *ImageWithCaption)
function DescribeOptionalCaption(input: ImageWithCaption?) -> string {
    client TestClient
    prompt #"Describe optional captioned image: {{ input }}"#
}

// Edge case P1: list of class params with media (ImageWithCaption[] -> []ImageWithCaption)
function DescribeCaptionList(inputs: ImageWithCaption[]) -> string {
    client TestClient
    prompt #"Describe captioned images: {{ inputs }}"#
}

// Regression test: parameter named "context" that shadows Go's context package.
// BAML preserves user parameter names verbatim in generated Go code. When a parameter
// is named "context", it shadows the "context" standard library import, causing
// compilation errors in functions_build_request.go / functions_build_request_stream.go
// (and previously in functions_parse.go / functions_parse_stream.go).
// The context_fix hack (cmd/hacks/hacks/context_fix.go) works around this by rewriting
// the generated code. If this function causes a build failure, the hack needs updating.
// See: https://github.com/BoundaryML/baml/issues/2393
function GetContextInput(context: string) -> string {
    client TestClient
    prompt #"Process context: {{ context }}"#
}

// Self-referential types: recursive tree without media
function ParseTree(input: string) -> TreeNode {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Parse as a tree: {{ input }}"#
}

// Self-referential types: recursive tree with media
function ParseMediaTree(input: string) -> MediaTreeNode {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Parse as a media tree: {{ input }}"#
}

// Self-referential types: JSON-like recursive union
function ParseJson(input: string) -> JsonContainer {
    client TestClient
    prompt #"{{ ctx.output_format }}
    Parse as JSON: {{ input }}"#
}
