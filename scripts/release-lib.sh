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

# release_dep_allows_pseudo <full_dep_path>
#
# Returns 0 (true) iff <full_dep_path> is a first-party module that
# may sit at a `v0.0.0-...` pseudo-version in root's go.mod, 1
# otherwise. The set is the inverse of release_required_modules:
#
#   - <prefix>/dynclient                  — public consumer-facing
#                                           module, published with a
#                                           tag, but root resolves it
#                                           through a local replace
#                                           and the require version
#                                           is a placeholder.
#   - <prefix>/pool                       — workspace-only.
#   - <prefix>/adapters/adapter_v*        — every server-only adapter
#                                           module, also workspace-
#                                           only.
#
# Note that <prefix>/dynclient/baml-patched is NOT in this set: it's
# a published, indirectly-required module in root with no local
# replace, so it must be at exact v${version} like the other
# published modules. The exact-prefix match below distinguishes
# "dynclient" from "dynclient/baml-patched".
release_dep_allows_pseudo() {
    local dep="$1"
    local prefix="$release_first_party_prefix"
    case "$dep" in
        "${prefix}/dynclient"|"${prefix}/pool")
            return 0
            ;;
        "${prefix}/adapters/adapter_v"*)
            return 0
            ;;
    esac
    return 1
}

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

# release_validate_module_requires <module_label> <expected_version> [allow_pseudo]
#
# Reads go.mod from stdin and validates that every first-party
# require line is exactly <expected_version>. Pseudo-versions and any
# non-`v0.0.<positive-int>` require fail by default.
#
# Pass <allow_pseudo>=true to permit `v0.0.0-...` requires alongside
# <expected_version>, but ONLY for first-party modules that
# release_dep_allows_pseudo accepts (the workspace-only set:
# adapter_v*, dynclient, pool). A pseudo-version for a published
# module (anything in release_required_modules) still fails even when
# allow_pseudo=true — those modules must always move forward to
# exact <expected_version> at each release. The published-module
# loop in release-prep.sh passes the default false and the root
# validator passes true; both rely on the same per-dep gate so a
# pseudo accidentally landing on bamlutils / common / introspected /
# worker / workerplugin / dynclient/baml-patched in root still
# trips validation.
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
#                     Emitted even when allow_pseudo=true so callers
#                     that want to distinguish "clean release" from
#                     "release with surviving pseudos" still can; the
#                     return code is 0 in that case but the channel
#                     records the lines.
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
    local allow_pseudo="${3:-false}"

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
            release_last_failure_lines+=$'pseudo\t'"${dep_version}"$'\n'
            # allow_pseudo is the call-site gate ("any pseudo at all
            # acceptable here?"); release_dep_allows_pseudo is the
            # per-dep gate ("is THIS first-party module workspace-only?").
            # Both must be true for a pseudo-version to pass. This stops
            # an accidental pseudo on a published module (e.g. bamlutils
            # in root) from sneaking through under allow_pseudo=true.
            if [ "$allow_pseudo" = true ] && release_dep_allows_pseudo "$dep"; then
                continue
            fi
            echo "  pseudo-version not allowed: ${dep} ${dep_version}" >&2
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
