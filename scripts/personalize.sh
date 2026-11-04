#!/usr/bin/env bash
# Replace the placeholders with your details, once, before the first push.
#   ./scripts/personalize.sh <github-username> "<Your Name>" <email> <linkedin-handle>
set -euo pipefail
if [ $# -ne 4 ]; then
  echo "usage: $0 <github-username> \"<Your Name>\" <email> <linkedin-handle>" >&2
  exit 1
fi
user="$1"; name="$2"; email="$3"; li="$4"
files=$(grep -rlE 'your-handle|Your Name|you@example.com|your-linkedin' --exclude-dir=.git --exclude=personalize.sh .)
for f in $files; do
  sed -i.bak -e "s#your-handle#${user}#g" -e "s#Your Name#${name}#g" -e "s#you@example.com#${email}#g" -e "s#your-linkedin#${li}#g" "$f"
  rm -f "$f.bak"
done
gofmt -l . >/dev/null
go build ./... && echo "Done. Module path is now github.com/${user}/recura."
