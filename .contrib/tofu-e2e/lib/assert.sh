#!/bin/sh
# Assertion helpers for the tofu e2e rig. Each assert returns non-zero on
# failure after printing the disagreement. Sourced by run-e2e.sh.

# assert_contains <needle> <haystack> — pass when haystack contains needle
assert_contains() {
    needle=$1
    haystack=$2
    case "$haystack" in
        *"$needle"*) return 0 ;;
    esac
    echo "assert_contains: want '$needle' in: $haystack" >&2
    return 1
}

# assert_file_contains <needle> <path>
assert_file_contains() {
    grep -q "$1" "$2" || { echo "assert_file_contains: want '$1' in $2" >&2; return 1; }
}

# assert_file_missing <path>
assert_file_missing() {
    [ ! -e "$1" ] || { echo "assert_file_missing: $1 exists" >&2; return 1; }
}

# assert_exit <want> <got> <label>
assert_exit() {
    if [ "$1" != "$2" ]; then
        echo "assert_exit: $3: want $1 got $2" >&2
        return 1
    fi
}
