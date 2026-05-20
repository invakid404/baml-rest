# release-lib.sh — shared helpers for scripts/release-prep.sh and
# scripts/release-go-tags.sh. Source from another script:
#
#   source "$(dirname "${BASH_SOURCE[0]}")/release-lib.sh"
#
# The two scripts validate the same shape (first-party requires must
# be at exactly `v${version}`) but feed it different go.mod sources:
# release-prep.sh checks the current worktree, release-go-tags.sh
# checks `git show <tag>:<path>/go.mod`. Keeping the awk + parsing
# logic here means a fix to one validator can't drift away from the
# other.

# First-party module prefix and the set of published modules that
# carry release tags. Edit here if a future release adds or removes a
# published Go module. The root module, pool, and the server-only
# adapters/adapter_v* modules are deliberately excluded — see PR #289
# discussion and RELEASE.md.
release_first_party_prefix="github.com/invakid404/baml-rest"
release_required_modules=(
    "adapters/common"
    "bamlutils"
    "dynclient"
    "dynclient/baml-patched"
    "introspected"
    "worker"
    "workerplugin"
)

# release_extract_first_party_requires
#
# Reads a go.mod from stdin and prints first-party `require` lines
# (without the leading `require` keyword) to stdout.
#
# A first-party require inside a `replace (...)`, `exclude (...)`, or
# `retract (...)` block is intentionally ignored — the local
# `replace ../../bamlutils` directives present during release-prep
# look syntactically similar to a require line and must not be
# mistaken for one.
release_extract_first_party_requires() {
    awk -v prefix="$release_first_party_prefix" '
        function is_first_party(path) {
            return path == prefix || index(path, prefix "/") == 1
        }
        /^[[:space:]]*require[[:space:]]*\(/ { block = "require"; next }
        /^[[:space:]]*replace[[:space:]]*\(/ { block = "replace"; next }
        /^[[:space:]]*exclude[[:space:]]*\(/ { block = "exclude"; next }
        /^[[:space:]]*retract[[:space:]]*\(/ { block = "retract"; next }
        /^[[:space:]]*\)/ { block = ""; next }
        /^[[:space:]]*require[[:space:]]+/ {
            line = $0
            sub(/^[[:space:]]*require[[:space:]]+/, "", line)
            split(line, fields, /[[:space:]]+/)
            if (is_first_party(fields[1])) print line
            next
        }
        block == "require" {
            line = $0
            sub(/^[[:space:]]*/, "", line)
            split(line, fields, /[[:space:]]+/)
            if (is_first_party(fields[1])) print line
        }
    '
}

# release_validate_module_requires <module_label> <expected_version>
#
# Reads go.mod from stdin and validates that every first-party
# require line is exactly <expected_version>. Pseudo-versions and any
# non-`v0.0.<positive-int>` require fail too.
#
# Output:
#   - returns 0 if clean, 1 otherwise.
#   - on failure, emits one indented offending line per failure to
#     stderr (no leading module label — the caller prints that once,
#     so the operator sees every fix needed in one pass).
#
# For every failed line in a module, appends a record to the global
# variable release_last_failure_lines. The accumulator is a
# newline-delimited list of `kind<TAB>value` rows where kind is one of:
#   - "pseudo"      — a v0.0.0-... pseudo-version require survived.
#   - "stale"       — a clean v0.0.<N> require that points at the
#                     wrong (most often previous) release; this is
#                     the canonical "release-prep was skipped" shape.
#   - "malformed"   — a require whose version doesn't even match the
#                     v0.0.<positive-int> shape.
#   - "unparseable" — a require line that doesn't parse.
# For "stale" rows, value is the offending version string (so callers
# can collect stale_versions directly from this channel); for other
# kinds value is the offending version (or the raw line for
# "unparseable"), retained only as a debugging aid.
#
# Callers MUST reset release_last_failure_lines to "" before each
# module so per-module classification stays clean. The accumulator
# updates state in the current shell, so callers must run the
# validator via process substitution / file redirection (not a pipe)
# to keep variable assignments in scope.
release_last_failure_lines=""

release_validate_module_requires() {
    local module_label="$1"
    local expected_version="$2"

    local first_party_lines
    first_party_lines="$(release_extract_first_party_requires)"

    if [ -z "$first_party_lines" ]; then
        return 0
    fi

    local bad=0
    local line dep dep_version stripped
    while IFS= read -r line; do
        # Strip any trailing `// indirect` (or other) comment so the
        # version field is the last whitespace-separated token.
        stripped="${line%%//*}"
        # shellcheck disable=SC2206
        local fields=($stripped)
        if [ "${#fields[@]}" -lt 2 ]; then
            echo "  bad require line (cannot parse): ${line}" >&2
            release_last_failure_lines+=$'unparseable\t'"${line}"$'\n'
            bad=1
            continue
        fi
        dep="${fields[0]}"
        dep_version="${fields[1]}"

        if [[ "$dep_version" == v0.0.0-* ]]; then
            echo "  pseudo-version not allowed: ${dep} ${dep_version}" >&2
            release_last_failure_lines+=$'pseudo\t'"${dep_version}"$'\n'
            bad=1
            continue
        fi
        if [[ ! "$dep_version" =~ ^v0\.0\.[1-9][0-9]*$ ]]; then
            echo "  version must match v0.0.<positive-integer>: ${dep} ${dep_version}" >&2
            release_last_failure_lines+=$'malformed\t'"${dep_version}"$'\n'
            bad=1
            continue
        fi
        if [ "$dep_version" != "$expected_version" ]; then
            echo "  expected ${expected_version}, got: ${dep} ${dep_version}" >&2
            release_last_failure_lines+=$'stale\t'"${dep_version}"$'\n'
            bad=1
            continue
        fi
    done <<< "$first_party_lines"

    return "$bad"
}

# release_find_repo_root <start_dir> <sentinel>
#
# Walks up from <start_dir> until a directory containing <sentinel>
# is found, then prints that directory. <sentinel> is a filename or
# directory name relative to each candidate (e.g. ".git", "go.work").
# Prints nothing and returns 1 if no ancestor matches.
release_find_repo_root() {
    local dir="$1"
    local sentinel="$2"
    while [ "$dir" != "/" ]; do
        if [ -e "$dir/$sentinel" ]; then
            printf '%s\n' "$dir"
            return 0
        fi
        dir="$(dirname "$dir")"
    done
    return 1
}
