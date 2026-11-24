#!/bin/sh
# undoguard demo: a command that escapes the project is blocked, and a
# deletion inside the project is allowed, snapshotted and undone.
#
# Everything happens in a throwaway home directory, so the real one is
# never touched. Needs undoguard on PATH, or UNDOGUARD=/path/to/binary.
# Exits non-zero if undoguard does not behave as shown.
set -eu

UNDOGUARD=${UNDOGUARD:-undoguard}
if ! command -v "$UNDOGUARD" >/dev/null 2>&1; then
	echo "demo: $UNDOGUARD not found; build it or set UNDOGUARD" >&2
	exit 1
fi

fail() {
	echo "demo: FAILED: $*" >&2
	exit 1
}

# Show a command, then run it. Its exit code ends up in $code.
show() {
	printf '\n$ %s\n' "$1"
	shift
	set +e
	"$@"
	code=$?
	set -e
}

work=$(mktemp -d)
# The physical path, so that the output does not depend on /tmp or
# /var being symlinks.
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM

HOME="$work/home"
UNDOGUARD_HOME="$HOME/.local/share/undoguard"
UNDOGUARD_CONFIG_DIR="$HOME/.config/undoguard"
export HOME UNDOGUARD_HOME UNDOGUARD_CONFIG_DIR
unset XDG_DATA_HOME XDG_CONFIG_HOME UNDOGUARD_PROJECT CLAUDE_PROJECT_DIR \
	GEMINI_PROJECT_DIR UNDOGUARD_SESSION UNDOGUARD_AGENT UNDOGUARD_ACTIVE

mkdir -p "$HOME/important" "$HOME/project/build"
echo "the only copy" >"$HOME/important/notes.txt"
echo "compiled output" >"$HOME/project/build/app.bin"
printf 'int main(void) { return 0; }\n' >"$HOME/project/main.c"
cd "$HOME/project"

# 1. The path looks like it is inside the project, but resolves to
#    ~/important, which is in the home directory: denied.
show "undoguard check 'rm -rf ~/project/../important'" \
	"$UNDOGUARD" check 'rm -rf ~/project/../important'
[ "$code" -eq 2 ] || fail "check should deny with exit code 2, got $code"

# 2. The shell wrapper refuses to run it, the way an agent's shell would.
show "undoguard exec --shell /bin/sh -- -c 'rm -rf ~/project/../important'" \
	"$UNDOGUARD" exec --shell /bin/sh -- -c 'rm -rf ~/project/../important'
[ "$code" -eq 126 ] || fail "exec should block with exit code 126, got $code"
[ -f "$HOME/important/notes.txt" ] || fail "~/important was deleted"

# 3. Deleting build output inside the project is allowed, after a
#    snapshot.
show "undoguard check 'rm -rf build'" \
	"$UNDOGUARD" check 'rm -rf build'
[ "$code" -eq 0 ] || fail "check should allow with exit code 0, got $code"

show "undoguard exec --shell /bin/sh -- -c 'rm -rf build'" \
	"$UNDOGUARD" exec --shell /bin/sh -- -c 'rm -rf build'
[ "$code" -eq 0 ] || fail "the allowed command failed with exit code $code"
[ ! -e build ] || fail "build should be gone"

show "undoguard log" "$UNDOGUARD" log
[ "$code" -eq 0 ] || fail "log failed with exit code $code"

# 4. One command brings it back.
show "undoguard undo" "$UNDOGUARD" undo
[ "$code" -eq 0 ] || fail "undo failed with exit code $code"
[ "$(cat build/app.bin)" = "compiled output" ] || fail "build/app.bin was not restored"

printf '\n$ cat build/app.bin\n'
cat build/app.bin
echo
echo "demo: all steps behaved as expected"
