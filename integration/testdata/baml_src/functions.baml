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
