#!/bin/sh
# A shell that checks every command line before running it.
#
# Point a tool that runs shell commands at this file instead of the real
# shell, for example SHELL=/path/to/shell-wrapper.sh, or install it as
# "bash" early on the PATH of an agent. `undoguard run -- <agent>` does
# the same with temporary wrappers for sh, bash, zsh, dash and ksh.
#
# A blocked command exits with status 126 and the reason on stderr.
# Interactive shells and script files run unchecked: there is no command
# line to look at.
exec undoguard exec --shell /bin/bash -- "$@"
