#!/usr/bin/env bash
# Let editors resolve Bazel-generated Go packages without checking in generated code.
set -euo pipefail
exec bazel run -- @rules_go//go/tools/gopackagesdriver "$@"
