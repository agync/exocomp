#!/bin/sh
# The repo-specific commit-msg rules conventional-pre-commit cannot express:
# title <= 52 chars (skipping git-generated subjects), no disallowed trailers,
# no AI-slop emoji. Invoked by pre-commit at commit-msg stage with the message
# file as $1.
set -eu

msg_file=$1
subject=$(head -n 1 "$msg_file")

fail() {
    echo "commit-msg-extras: $1" >&2
    exit 1
}

# Git-generated subjects (merge, revert) and rebase-autosquash markers are
# exempt from the length rule — conventional-pre-commit already passes the
# autosquash prefixes and merge subjects, and `revert` is in the allowed type
# list. The trailer and emoji guards still apply to the full message.
git_generated=0
case "$subject" in
    "Merge "*|"Revert \""*|"fixup! "*|"squash! "*|"amend! "*) git_generated=1 ;;
esac

if [ "$git_generated" -eq 0 ]; then
    subject_len=$(printf '%s' "$subject" | wc -m | tr -d ' ')
    if [ "$subject_len" -gt 52 ]; then
        fail "title exceeds 52 chars ($subject_len): $subject"
    fi
fi

if grep -Eiq '^(Co-Authored-By|Signed-off-by|Claude-Session|Generated-with|Generated with):?' "$msg_file"; then
    fail "message contains a disallowed trailer (Co-Authored-By/Signed-off-by/Claude-Session/Generated-with)"
fi

# Conservative check for the common "AI slop" emoji: robot, sparkles, rocket
# and a few celebratory glyphs — not a full unicode-emoji class.
if echo x | grep -qP 'x' 2>/dev/null; then
    if grep -qP '[\x{1F916}\x{2728}\x{1F680}\x{1F31F}\x{2705}\x{274C}\x{1F389}\x{1F4A1}]' "$msg_file"; then
        fail "message contains an emoji"
    fi
else
    # No PCRE grep on this host: match the same glyphs via their UTF-8 bytes so
    # the guard degrades to a byte-pattern check instead of silently passing.
    if grep -q "$(printf '\xf0\x9f\xa4\x96\|\xe2\x9c\xa8\|\xf0\x9f\x9a\x80\|\xf0\x9f\x8c\x9f\|\xe2\x9c\x85\|\xe2\x9d\x8c\|\xf0\x9f\x8e\x89\|\xf0\x9f\x92\xa1')" "$msg_file"; then
        fail "message contains an emoji"
    fi
fi

exit 0
