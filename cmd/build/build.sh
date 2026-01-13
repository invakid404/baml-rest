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

# Get target architecture for platform-specific caching
# TARGETARCH is set by Docker during multi-arch builds (e.g., amd64, arm64)
TARGETARCH="${TARGETARCH:-$(uname -m)}"
# Normalize architecture names
case "${TARGETARCH}" in
    x86_64) TARGETARCH="amd64" ;;
    aarch64) TARGETARCH="arm64" ;;
esac

# Use a version and architecture-specific location in the cache mount for BAML downloads during build
# This ensures the shared library is cached across builds and avoids version/architecture conflicts
BAML_CACHE_BUILD="${CACHE_DIR}/baml-shared-lib/${BAML_VERSION}/${TARGETARCH}"

# Configure unified caching (npm cache is architecture-specific due to native bindings)
export NPM_CONFIG_CACHE="${CACHE_DIR}/npm/${TARGETARCH}"
export GOMODCACHE="${CACHE_DIR}/go/mod"
export GOCACHE="${CACHE_DIR}/go/build"
export BAML_CACHE_DIR="${BAML_CACHE_BUILD}"

# Create cache directories if they don't exist
mkdir -p "${NPM_CONFIG_CACHE}"
mkdir -p "${GOMODCACHE}"
mkdir -p "${GOCACHE}"
mkdir -p "${BAML_CACHE_DIR}"

# Handle custom BAML lib if provided
CUSTOM_BAML_LIB_PATH="${USER_CONTEXT_PATH}/custom_baml_lib.so"
if [ -f "${CUSTOM_BAML_LIB_PATH}" ]; then
    echo ""
    echo "=== Custom BAML Library Detected ==="

    # Determine the correct filename based on architecture
    case "${TARGETARCH}" in
        amd64)
            BAML_LIB_FILENAME="libbaml_cffi-x86_64-unknown-linux-gnu.so"
            ;;
        arm64)
            BAML_LIB_FILENAME="libbaml_cffi-aarch64-unknown-linux-gnu.so"
            ;;
        *)
            echo "ERROR: Custom BAML lib not supported for architecture: ${TARGETARCH}"
            exit 1
            ;;
    esac

    echo "Installing custom BAML lib as: ${BAML_LIB_FILENAME}"
    cp "${CUSTOM_BAML_LIB_PATH}" "${BAML_CACHE_DIR}/${BAML_LIB_FILENAME}"
    echo "Custom BAML lib installed to: ${BAML_CACHE_DIR}/${BAML_LIB_FILENAME}"
    echo ""
fi

echo "============================================"
echo "BAML REST API Build Script"
echo "============================================"
echo "Target Architecture: ${TARGETARCH}"
echo "BAML Version: ${BAML_VERSION}"
echo "Adapter Version: ${ADAPTER_VERSION}"
echo "User Context: ${USER_CONTEXT_PATH}"
echo "Output Path: ${OUTPUT_PATH}"
echo "Cache Directory: ${CACHE_DIR}"
echo "NPM Cache: ${NPM_CONFIG_CACHE}"
echo "BAML Cache (build): ${BAML_CACHE_BUILD}"
echo "BAML Cache (final): ${BAML_CACHE_FINAL}"
if [ -n "${BAML_LIB_FILENAME:-}" ]; then
    echo "Custom BAML Lib: ${BAML_LIB_FILENAME}"
fi
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
npx "@boundaryml/baml@${BAML_VERSION}" generate

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
pushd baml_client
go mod init github.com/invakid404/baml-rest/baml_client
popd

# Add generated client to Go workspace
echo "Adding BAML client to Go workspace..."
go work use ./baml_client

# Clean up unused adapters to avoid version conflicts
echo ""
echo "=== Cleaning up unused adapters ==="
SELECTED_ADAPTER=$(basename "${ADAPTER_VERSION}")
echo "Selected adapter: ${SELECTED_ADAPTER}"

# Delete all adapter_v* directories except the selected one
echo "Removing unused adapter directories from adapters/..."
for adapter_dir in adapters/adapter_v*/; do
    if [ -d "${adapter_dir}" ]; then
        adapter_name=$(basename "${adapter_dir}")
        if [ "${adapter_name}" != "${SELECTED_ADAPTER}" ]; then
            echo "  Deleting ${adapter_dir}..."
            rm -rf "${adapter_dir}"
        else
            echo "  Keeping ${adapter_dir}"
        fi
    fi
done

# Remove replace statements from go.mod for deleted adapters
echo "Cleaning up go.mod replace statements..."
tmp_file=$(mktemp)
in_replace_block=0

while IFS= read -r line; do
    # Detect start of replace block
    if [[ "$line" =~ ^replace[[:space:]]*\( ]]; then
        in_replace_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of replace block
    elif [[ "$in_replace_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_replace_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the replace block
    elif [[ "$in_replace_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* directory
        if [[ "$line" =~ adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing replace for: ${adapter_in_line}"
            fi
        else
            # Not an adapter line, keep it (common, bamlutils, introspected, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in replace block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.mod

mv "$tmp_file" go.mod

# Remove require statements from go.mod for deleted adapters
echo "Cleaning up go.mod require statements..."
tmp_file=$(mktemp)
in_require_block=0

while IFS= read -r line; do
    # Detect start of require block
    if [[ "$line" =~ ^require[[:space:]]*\( ]]; then
        in_require_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of require block
    elif [[ "$in_require_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_require_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the require block
    elif [[ "$in_require_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* module
        if [[ "$line" =~ github\.com/invakid404/baml-rest/adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing require for: ${adapter_in_line}"
            fi
        else
            # Not an adapter_v* line, keep it (common, bamlutils, introspected, other deps, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in require block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.mod

mv "$tmp_file" go.mod

# Remove use statements from go.work for deleted adapters
echo "Cleaning up go.work use statements..."
tmp_file=$(mktemp)
in_use_block=0

while IFS= read -r line; do
    # Detect start of use block
    if [[ "$line" =~ ^use[[:space:]]*\( ]]; then
        in_use_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of use block
    elif [[ "$in_use_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_use_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the use block
    elif [[ "$in_use_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* directory
        if [[ "$line" =~ adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing use for: ${adapter_in_line}"
            fi
        else
            # Not an adapter line, keep it (., common, bamlutils, introspected, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in use block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.work

mv "$tmp_file" go.work

# Regenerate embed.go to reflect the deleted adapters
echo "Regenerating embed.go..."
go run cmd/embed/main.go

echo "Adapter cleanup complete!"
echo ""

# Get BAML dependency
echo "Getting BAML Go dependency (github.com/boundaryml/baml@${BAML_VERSION})..."
go get "github.com/boundaryml/baml@${BAML_VERSION}"

# Sync Go workspace
echo "Syncing Go workspace..."
go work sync

# Ensure baml_client uses the correct BAML version
pushd baml_client
echo "Setting BAML version in baml_client to ${BAML_VERSION}..."
go get "github.com/boundaryml/baml@${BAML_VERSION}"
popd

# Run hacks to patch generated BAML client
echo "Running hacks..."
go run cmd/hacks/main.go --baml-client-dir ./baml_client --baml-version "${BAML_VERSION}"

# Run introspection
echo "Running introspection..."
go run cmd/introspect/main.go

# Format and organize imports
echo "Formatting code and organizing imports..."
gofmt -w .
goimports -w .

# Run adapter
echo "Running adapter (${ADAPTER_VERSION})..."
go run "${ADAPTER_VERSION}/cmd/main.go"

# Build worker binary first (this imports baml and loads the shared library)
echo "Building worker binary..."
go build -o cmd/serve/worker cmd/worker/main.go

# Generate OpenAPI schema (this also imports baml)
echo "Generating OpenAPI schema..."
go run cmd/schema/main.go cmd/serve/openapi.json

# Build final binary (embeds worker and schema, doesn't import baml directly)
echo "Building final binary..."
go build -o baml-rest cmd/serve/main.go

# Clean up intermediate files from cmd/serve (they're embedded now)
rm -f cmd/serve/worker cmd/serve/openapi.json

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
