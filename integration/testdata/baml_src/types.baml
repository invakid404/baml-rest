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
