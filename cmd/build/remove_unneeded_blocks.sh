#!/usr/bin/env bash

KEYWORDS='generator|test'

find . -type f -name '*.baml' -print0 | while IFS= read -r -d '' file; do
  gawk -v kw="$KEYWORDS" '
    BEGIN { pat = "^[ \t]*(" kw ")[ \t][^{]*\\{" }
    $0 ~ pat {
      in_block = 1
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level = opens - closes
      if (level <= 0) in_block = 0
      next
    }
    in_block {
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level += opens - closes
      if (level <= 0) in_block = 0
      next
    }
    { print }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
done
