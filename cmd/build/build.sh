#!/usr/bin/env bash
set -euo pipefail

# Unified Build Script for BAML REST API
# This script performs the complete build process: Node.js client generation + Go compilation

# Required environment variables:
# - BAML_VERSION: Version of BAML to use (e.g., "0.204.0")
# - ADAPTER_VERSION: Adapter version to use (e.g., "v0.204.0")
# - USER_CONTEXT_PATH: Path to the user's context directory containing baml_src
#
# Optional environment variables:
# - OUTPUT_PATH: Where to place the final binary (default: /output/baml-rest)
# - CACHE_DIR: Root cache directory (default: /cache)
# - BAML_CACHE_DIR: Final BAML cache directory (default: /baml-cache)

# Validate required environment variables
if [ -z "${BAML_VERSION:-}" ]; then
    echo "ERROR: BAML_VERSION environment variable is required"
    exit 1
fi

if [ -z "${ADAPTER_VERSION:-}" ]; then
    echo "ERROR: ADAPTER_VERSION environment variable is required"
    exit 1
fi

if [ -z "${USER_CONTEXT_PATH:-}" ]; then
    echo "ERROR: USER_CONTEXT_PATH environment variable is required"
    exit 1
fi

# Set defaults for optional variables
OUTPUT_PATH="${OUTPUT_PATH:-/output/baml-rest}"
CACHE_DIR="${CACHE_DIR:-/cache}"
BAML_CACHE_DIR="${BAML_CACHE_DIR:-/baml-cache}"

# Save the final BAML cache destination
BAML_CACHE_FINAL="${BAML_CACHE_DIR}"

# Use a version-specific location in the cache mount for BAML downloads during build
# This ensures the shared library is cached across builds and avoids version conflicts
BAML_CACHE_BUILD="${CACHE_DIR}/baml-shared-lib/${BAML_VERSION}"

# Configure unified caching
export NPM_CONFIG_CACHE="${CACHE_DIR}/npm"
export GOMODCACHE="${CACHE_DIR}/go/mod"
export GOCACHE="${CACHE_DIR}/go/build"
export BAML_CACHE_DIR="${BAML_CACHE_BUILD}"

# Create cache directories if they don't exist
mkdir -p "${NPM_CONFIG_CACHE}"
mkdir -p "${GOMODCACHE}"
mkdir -p "${GOCACHE}"
mkdir -p "${BAML_CACHE_DIR}"

echo "============================================"
echo "BAML REST API Build Script"
echo "============================================"
echo "BAML Version: ${BAML_VERSION}"
echo "Adapter Version: ${ADAPTER_VERSION}"
echo "User Context: ${USER_CONTEXT_PATH}"
echo "Output Path: ${OUTPUT_PATH}"
echo "Cache Directory: ${CACHE_DIR}"
echo "BAML Cache (build): ${BAML_CACHE_BUILD}"
echo "BAML Cache (final): ${BAML_CACHE_FINAL}"
echo "============================================"

# Create working directory structure
WORK_DIR="$(mktemp -d)"
if [ "${KEEP_SOURCE:-false}" != "true" ]; then
    trap "rm -rf ${WORK_DIR}" EXIT
fi

BAML_WORK="${WORK_DIR}/baml"
BUILD_WORK="${WORK_DIR}/build"

mkdir -p "${BAML_WORK}"
mkdir -p "${BUILD_WORK}"

echo ""
echo "=== Stage 1: Node.js Client Generation ==="
echo ""

# Check for required tools (Node.js stage)
if ! command -v node &> /dev/null; then
    echo "ERROR: node is not installed or not in PATH"
    exit 1
fi

if ! command -v npx &> /dev/null; then
    echo "ERROR: npx is not installed or not in PATH"
    exit 1
fi

# Copy user's baml_src to working directory
echo "Copying baml_src from ${USER_CONTEXT_PATH}..."
cp -r "${USER_CONTEXT_PATH}/baml_src" "${BAML_WORK}/baml_src"

# Change to baml working directory
cd "${BAML_WORK}"

# Run remove_unneeded_blocks.sh script
echo "Removing unneeded blocks from .baml files..."
KEYWORDS='generator|test'

find baml_src -type f -name '*.baml' -print0 | while IFS= read -r -d '' file; do
  gawk -v kw="$KEYWORDS" '
    BEGIN { pat = "^[ \t]*(" kw ")[ \t][^{]*\\{" }
    $0 ~ pat {
      in_block = 1
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level = opens - closes
      if (level <= 0) in_block = 0
      next
    }
    in_block {
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level += opens - closes
      if (level <= 0) in_block = 0
      next
    }
    { print }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
done

# Render clients.baml template using envsubst
echo "Rendering clients.baml template..."
export OUTPUT_DIR="../baml_rest_generated"
cat > clients.baml.template <<'EOF'
generator baml_rest_target {
  output_type "go"
  output_dir "${OUTPUT_DIR}"
  version "${BAML_VERSION}"
  client_package_name "github.com/invakid404/baml-rest"
}
EOF

cat clients.baml.template | envsubst | tee baml_src/baml_rest_client.baml

# Generate BAML client
echo "Running BAML client generation (npx @boundaryml/baml@${BAML_VERSION} generate)..."
npx @boundaryml/baml@${BAML_VERSION} generate

echo ""
echo "=== Stage 2: Go Build ==="
echo ""

# Check for required tools (Go stage)
if ! command -v go &> /dev/null; then
    echo "ERROR: go is not installed or not in PATH"
    exit 1
fi

# Install goimports if not available
if ! command -v goimports &> /dev/null; then
    echo "Installing goimports..."
    go install golang.org/x/tools/cmd/goimports@latest
fi

# Copy baml_rest sources to build directory
echo "Copying baml_rest sources to build directory..."
cp -r "${USER_CONTEXT_PATH}/baml_rest" "${BUILD_WORK}/baml_rest"

# Change to build working directory
cd "${BUILD_WORK}/baml_rest"

# Copy generated BAML client
echo "Copying generated BAML client..."
cp -r "${BAML_WORK}/baml_rest_generated/baml_client" ./baml_client

# Initialize Go module for generated client
echo "Initializing Go module for BAML client..."
cd baml_client
go mod init github.com/invakid404/baml-rest/baml_client
cd ..

# Add generated client to Go workspace
echo "Adding BAML client to Go workspace..."
go work use ./baml_client

# Get BAML dependency
echo "Getting BAML Go dependency (github.com/boundaryml/baml@${BAML_VERSION})..."
go get github.com/boundaryml/baml@${BAML_VERSION}

# Sync Go workspace
echo "Syncing Go workspace..."
go work sync

# Run introspection
echo "Running introspection..."
go run cmd/introspect/main.go

# Format and organize imports
echo "Formatting code and organizing imports..."
gofmt -w .
goimports -w .

# Run adapter
echo "Running adapter (${ADAPTER_VERSION})..."
go run ${ADAPTER_VERSION}/cmd/main.go

# Build final binary
echo "Building final binary..."
go build -o baml-rest cmd/serve/main.go

# Create output directory and copy binary
OUTPUT_DIR="$(dirname "${OUTPUT_PATH}")"
mkdir -p "${OUTPUT_DIR}"

echo "Copying binary to ${OUTPUT_PATH}..."
cp baml-rest "${OUTPUT_PATH}"

# Handle source preservation if KEEP_SOURCE is enabled
if [ "${KEEP_SOURCE:-false}" = "true" ]; then
    echo ""
    echo "KEEP_SOURCE enabled - preserving generated source files..."

    KEEP_SOURCE_DIR="${KEEP_SOURCE_DIR:-/baml-rest-generated-src}"

    # Attempt to create the directory and all parent directories
    if mkdir -p "${KEEP_SOURCE_DIR}" 2>/dev/null && [ -d "${KEEP_SOURCE_DIR}" ] && [ -w "${KEEP_SOURCE_DIR}" ]; then
        # Successfully created/verified directory with write permissions
        echo "Copying generated source to ${KEEP_SOURCE_DIR}..."
        if cp -r . "${KEEP_SOURCE_DIR}/" 2>/dev/null; then
            echo "Generated source files saved to ${KEEP_SOURCE_DIR}"
        else
            echo "WARNING: Failed to copy source files to ${KEEP_SOURCE_DIR}"
            echo "Generated source files preserved at: ${BUILD_WORK}/baml_rest"
            KEEP_SOURCE_DIR="${BUILD_WORK}/baml_rest"
        fi
    else
        # Cannot create directory or insufficient permissions - fall back to temp directory
        echo "Cannot write to ${KEEP_SOURCE_DIR} (permissions or invalid path)"
        echo "Generated source files preserved at: ${BUILD_WORK}/baml_rest"
        KEEP_SOURCE_DIR="${BUILD_WORK}/baml_rest"
    fi
fi

# Copy BAML cache from build location to final destination
echo ""
echo "Copying BAML cache from build location to final destination..."
mkdir -p "${BAML_CACHE_FINAL}"
if [ -d "${BAML_CACHE_BUILD}" ]; then
    cp -r "${BAML_CACHE_BUILD}/." "${BAML_CACHE_FINAL}/" || echo "WARNING: Failed to copy BAML cache to final destination"
    echo "BAML cache copied to: ${BAML_CACHE_FINAL}"
else
    echo "WARNING: BAML cache build directory does not exist: ${BAML_CACHE_BUILD}"
fi

echo ""
echo "============================================"
echo "Build completed successfully!"
echo "Binary location: ${OUTPUT_PATH}"
if [ "${KEEP_SOURCE:-false}" = "true" ]; then
    echo "Generated source: ${KEEP_SOURCE_DIR}"
fi
echo "BAML cache location: ${BAML_CACHE_FINAL}"
echo "============================================"
