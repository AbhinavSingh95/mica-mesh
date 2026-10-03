#!/usr/bin/env bash
set -euo pipefail

gofmt=$1
buildifier=$2
shift 2

go_files=()
build_files=()
for source in "$@"; do
    case "$source" in
        *.go) go_files+=("$source") ;;
        *.bazel|*.bzl|*/BUILD|BUILD) build_files+=("$source") ;;
    esac
done

unformatted=$("$gofmt" -l "${go_files[@]}")
if [[ -n "$unformatted" ]]; then
    printf 'Run the pinned gofmt on:\n%s\n' "$unformatted" >&2
    exit 1
fi
"$buildifier" -mode=check "${build_files[@]}"
