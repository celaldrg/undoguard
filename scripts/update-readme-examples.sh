#!/bin/sh
# Builds undoguard, runs examples/demo.sh and copies its real output into
# README.md between the demo-output markers. Run it after changing what
# undoguard prints, and commit the result.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd -P)
readme="$repo/README.md"
start='<!-- demo-output:start -->'
end='<!-- demo-output:end -->'

grep -qF "$start" "$readme" || { echo "no start marker in README.md" >&2; exit 1; }
grep -qF "$end" "$readme" || { echo "no end marker in README.md" >&2; exit 1; }

tmp=$(mktemp -d)
tmp=$(cd "$tmp" && pwd -P)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

(cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$tmp/bin/undoguard" ./cmd/undoguard)

# The demo makes its throwaway home below TMPDIR, so any path that is not
# shown relative to that home would contain $tmp and is caught below.
mkdir -p "$tmp/t"
if ! TMPDIR="$tmp/t" UNDOGUARD="$tmp/bin/undoguard" sh "$repo/examples/demo.sh" >"$tmp/out.txt" 2>&1; then
	cat "$tmp/out.txt" >&2
	echo "the demo failed; README.md was not changed" >&2
	exit 1
fi
if grep -qF "$tmp" "$tmp/out.txt"; then
	echo "the demo output contains a local path; README.md was not changed" >&2
	exit 1
fi

awk -v out="$tmp/out.txt" -v start="$start" -v end="$end" '
	index($0, start) == 1 {
		print
		print "```console"
		while ((getline line < out) > 0) print line
		print "```"
		skip = 1
		next
	}
	index($0, end) == 1 { skip = 0 }
	!skip { print }
' "$readme" >"$tmp/README.md"

cp "$tmp/README.md" "$readme"
echo "README.md updated"
