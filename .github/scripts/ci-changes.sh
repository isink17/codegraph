#!/usr/bin/env bash
# CI change classification and the documentation-only checks.
#
#   ci-changes.sh classify
#       Prints docs_only=true or docs_only=false for GITHUB_OUTPUT. Reads
#       CI_EVENT (pull_request|push), CI_BASE and CI_HEAD (commit SHAs) and
#       CI_STRESS (true when the windows-stress label is present). Every
#       error, unknown event, missing commit or empty diff prints
#       docs_only=false, so the full suite runs.
#
#   ci-changes.sh check-docs
#       Same environment. Runs git diff --check over the change and checks
#       every added or modified Markdown file: valid UTF-8, closed code
#       fences, relative links that point at existing files.
#
# Only an explicit allowlist counts as documentation. Anything else,
# including Markdown elsewhere in the tree, needs the full suite.
set -euo pipefail

# is_docs_path PATH: the documentation-only allowlist.
is_docs_path() {
	case "$1" in
	CHANGELOG.md | README.md) return 0 ;;
	docs/*.md) return 0 ;;
	esac
	return 1
}

# classify_paths: reads NUL-separated paths on stdin and prints true only
# when there is at least one path and every path is on the allowlist.
classify_paths() {
	local path seen=0
	while IFS= read -r -d '' path; do
		seen=1
		if ! is_docs_path "$path"; then
			echo false
			return
		fi
	done
	if [ "$seen" = 1 ]; then echo true; else echo false; fi
}

# diff_range: the git revision range for the event, or nothing when it
# cannot be computed safely.
diff_range() {
	local zero=0000000000000000000000000000000000000000
	case "${CI_BASE:-}" in "" | "$zero") return 1 ;; esac
	[ -n "${CI_HEAD:-}" ] || return 1
	git cat-file -e "${CI_BASE}^{commit}" 2>/dev/null || return 1
	git cat-file -e "${CI_HEAD}^{commit}" 2>/dev/null || return 1
	case "${CI_EVENT:-}" in
	# The whole pull request: everything since the merge base.
	pull_request) echo "${CI_BASE}...${CI_HEAD}" ;;
	# Everything the push moved the branch by, including force pushes.
	push) echo "${CI_BASE}..${CI_HEAD}" ;;
	*) return 1 ;;
	esac
}

classify() {
	local range paths
	if [ "${CI_STRESS:-false}" = true ]; then
		echo "windows-stress label: full CI" >&2
		echo docs_only=false
		return
	fi
	if ! range=$(diff_range); then
		echo "no usable commit range (event=${CI_EVENT:-} base=${CI_BASE:-} head=${CI_HEAD:-}): full CI" >&2
		echo docs_only=false
		return
	fi
	# A temp file keeps a failing git diff from looking like an empty list.
	paths=$(mktemp)
	if ! git diff --no-renames --name-only -z "$range" >"$paths"; then
		rm -f "$paths"
		echo "git diff $range failed: full CI" >&2
		echo docs_only=false
		return
	fi
	echo "changed files ($range):" >&2
	tr '\0' '\n' <"$paths" >&2
	echo "docs_only=$(classify_paths <"$paths")"
	rm -f "$paths"
}

# check_fences FILE: every fenced code block is closed.
check_fences() {
	awk '
		{
			line = $0
			sub(/^ {0,3}/, "", line)
			if (open == "") {
				if (match(line, /^(```+|~~~+)/)) { open = substr(line, 1, RLENGTH); start = NR }
			} else if (match(line, /^(```+|~~~+)[ \t]*$/)) {
				fence = substr(line, 1, RLENGTH)
				sub(/[ \t]+$/, "", fence)
				if (substr(fence, 1, 1) == substr(open, 1, 1) && length(fence) >= length(open)) open = ""
			}
		}
		END { if (open != "") { printf "%s:%d: code fence never closed\n", FILENAME, start; exit 1 } }
	' "$1"
}

# check_links FILE: relative Markdown link targets outside code exist.
check_links() {
	local file=$1 dir target status=0
	dir=$(dirname "$file")
	while IFS= read -r target; do
		target=${target%%#*}
		[ -n "$target" ] || continue
		if [ ! -e "$dir/$target" ]; then
			echo "$file: broken relative link: $target"
			status=1
		fi
	done < <(awk '
		/^ {0,3}(```|~~~)/ { fenced = !fenced; next }
		fenced { next }
		{
			line = $0
			gsub(/`[^`]*`/, "", line)
			while (match(line, /\]\([^) \t]+/)) {
				target = substr(line, RSTART + 2, RLENGTH - 2)
				line = substr(line, RSTART + RLENGTH)
				if (target ~ /^[A-Za-z][A-Za-z0-9+.-]*:/ || target ~ /^#/ || target ~ /^</) continue
				print target
			}
		}
	' "$file")
	return $status
}

check_docs() {
	local range file status=0
	range=$(diff_range) || { echo "no usable commit range" >&2; return 1; }
	git diff --check "$range" || status=1
	while IFS= read -r -d '' file; do
		if ! is_docs_path "$file"; then
			echo "$file: not a documentation path" >&2
			status=1
			continue
		fi
		if ! iconv -f UTF-8 -t UTF-8 "$file" >/dev/null 2>&1; then
			echo "$file: not valid UTF-8"
			status=1
		fi
		check_fences "$file" || status=1
		check_links "$file" || status=1
	done < <(git diff --no-renames --name-only -z --diff-filter=d "$range")
	return $status
}

case "${1:-}" in
classify) classify ;;
check-docs) check_docs ;;
# Lets the test script load the functions without running a command.
source-only) ;;
*)
	echo "usage: $0 classify|check-docs" >&2
	exit 2
	;;
esac
