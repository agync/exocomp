#!/bin/sh
# Idempotent local-shim bootstrap: hooksPath, CLAUDE.md pointer, .claude/ symlinks.
set -eu

root=$(git rev-parse --show-toplevel)
cd "$root"

git config core.hooksPath .agents/hooks
echo "set core.hooksPath = .agents/hooks"

claude_md="$root/CLAUDE.md"
claude_md_text='Read AGENTS.md — it is the entrypoint and contract for this repository.'
if [ ! -e "$claude_md" ]; then
    printf '%s\n' "$claude_md_text" >"$claude_md"
    echo "created CLAUDE.md"
else
    echo "CLAUDE.md already exists, left untouched"
fi

mkdir -p "$root/.claude"
echo "ensured .claude/ exists"

link_shim() {
    name=$1
    target=$2
    link="$root/.claude/$name"

    if [ -L "$link" ]; then
        current=$(readlink "$link")
        if [ "$current" = "$target" ]; then
            echo ".claude/$name already links to $target"
            return 0
        fi
        echo "bootstrap: .claude/$name is a symlink to '$current', expected '$target' — refusing to overwrite" >&2
        exit 1
    fi

    if [ -e "$link" ]; then
        echo "bootstrap: .claude/$name exists and is not a symlink — refusing to overwrite" >&2
        exit 1
    fi

    ln -s "$target" "$link"
    echo "linked .claude/$name -> $target"
}

link_shim agents ../.agents/agents
link_shim skills ../.agents/skills
