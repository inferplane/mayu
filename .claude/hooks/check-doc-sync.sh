#!/bin/bash
# Detect documentation sync needs after file changes.
# Triggered by PostToolUse (Write|Edit) events.
# Walks parent directories up to (and including) the source root before warning,
# so a CLAUDE.md at a top-level Go source dir (e.g. internal/CLAUDE.md) satisfies
# all packages beneath it.

FILE_PATH="${1:-}"
[ -z "$FILE_PATH" ] && exit 0

# Go project source roots.
SOURCE_ROOTS="cmd internal providers pkg"

for ROOT in $SOURCE_ROOTS; do
    if [[ "$FILE_PATH" == ${ROOT}/* ]]; then
        DIR=$(dirname "$FILE_PATH")
        FOUND_CLAUDE=false
        CHECK_DIR="$DIR"
        while true; do
            if [ -f "$CHECK_DIR/CLAUDE.md" ]; then
                FOUND_CLAUDE=true
                break
            fi
            [ "$CHECK_DIR" = "$ROOT" ] && break
            CHECK_DIR=$(dirname "$CHECK_DIR")
        done
        if ! $FOUND_CLAUDE; then
            echo "[doc-sync] No CLAUDE.md covers $DIR (checked up to $ROOT/). Create module documentation."
        fi
        break
    fi
done

# Alert if no ADRs exist when source or architecture files change.
IS_SOURCE=false
for ROOT in $SOURCE_ROOTS; do
    [[ "$FILE_PATH" == ${ROOT}/* ]] && IS_SOURCE=true && break
done
if $IS_SOURCE || [[ "$FILE_PATH" == docs/architecture.md ]]; then
    ADR_COUNT=$(find docs/decisions -name 'ADR-*.md' -not -name '.template.md' 2>/dev/null | wc -l)
    if [ "$ADR_COUNT" -eq 0 ]; then
        echo "[doc-sync] No ADRs found. Record architectural decisions in docs/decisions/."
    fi
fi

# Alert if no runbooks exist when infrastructure files change.
if [[ "$FILE_PATH" == Dockerfile* ]] || [[ "$FILE_PATH" == charts/* ]] || [[ "$FILE_PATH" == *terraform* ]] || [[ "$FILE_PATH" == *cdk* ]]; then
    RUNBOOK_COUNT=$(find docs/runbooks -name '*.md' -not -name '.template.md' 2>/dev/null | wc -l)
    if [ "$RUNBOOK_COUNT" -eq 0 ]; then
        echo "[doc-sync] No runbooks found. Create operational runbooks for deployment/recovery."
    fi
fi
