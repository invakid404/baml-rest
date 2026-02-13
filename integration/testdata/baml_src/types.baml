// Simple types for testing basic functionality

class SimpleOutput {
    message string
}

// Complex types with multiple fields
class Person {
    name string
    age int
    email string?              // Optional field
    tags string[]              // Array field
}

// Nested types
class Address {
    street string
    city string
    zip string
}

class PersonWithAddress {
    name string
    address Address
}

// Union types
class SuccessResult {
    data string
}

class ErrorResult {
    error string
    code int
}

// Enum
enum Category {
    TECH
    BUSINESS
    OTHER
}

class CategorizedItem {
    name string
    category Category
}

// Dynamic type (for type_builder testing)
class DynamicOutput {
    base_field string
    @@dynamic                  // Allows dynamic properties
}

// Dynamic enum (for type_builder testing)
enum DynamicCategory {
    DEFAULT
    @@dynamic                  // Allows dynamic values
}

class DynamicEnumOutput {
    name string
    category DynamicCategory
}

// Classes containing media types (for nested media testing)
class ImageWithCaption {
    img image
    caption string
}

class DocumentBundle {
    cover image?
    document pdf
    title string
}

// Edge case: class with a list of media (image[])
class ImageGallery {
    images image[]
    title string
}

// Edge case: class with a list of optional media ((image | null)[])
class OptionalImageGallery {
    images (image | null)[]
    title string
}

// Self-referential types (recursive via type aliases)
// Tests cycle detection in codegen and proper handling at runtime

class TreeNode {
    value string
    children TreeNodeList?
}

type TreeNodeList = TreeNode[]

// Self-referential type with media (recursive + media in same structure)
class MediaTreeNode {
    label string
    thumbnail image?
    children MediaTreeNodeList?
}

type MediaTreeNodeList = MediaTreeNode[]

// JSON-like recursive type (deeply nested via type alias)
type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>

class JsonContainer {
    data JsonValue
}

// Comprehensive type for OpenAPI schema validation testing
// Combines: nested objects, optional fields, arrays, enums, and multiple primitive types
enum Priority {
    LOW
    MEDIUM
    HIGH
    CRITICAL
}

class Tag {
    name string
    value string?
}

class Metadata {
    created_by string
    priority Priority
    tags Tag[]
}

class ComprehensiveOutput {
    id int
    title string
    description string?           // Optional string
    score float                   // Float type
    is_active bool                // Boolean type
    metadata Metadata             // Nested object with enum and array
    related_ids int[]             // Array of primitives
    labels string[]?              // Optional array
}
