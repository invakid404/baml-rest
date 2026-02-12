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
