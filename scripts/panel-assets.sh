#!/usr/bin/env bash
# Regenerates the panel templates and stylesheet with pinned tools. The
# Tailwind standalone CLI is downloaded once into bin/ and checked against the
# checksum published with its release before it ever runs. With --check the
# script fails when the committed files differ from what the tools produce.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
templ_version=v0.3.906
tailwind_version=v4.3.5

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) asset=tailwindcss-linux-x64; sum=e47695cee3f0d90623a91f3939b08bdee610eb5ad99340adff9d4145b284d6cc ;;
  Linux-aarch64 | Linux-arm64) asset=tailwindcss-linux-arm64; sum=ffcd0d21dbd7c605dc12743ff0c0b47debc963c86b399674084c1c8f2c96ecad ;;
  Darwin-arm64) asset=tailwindcss-macos-arm64; sum=f3f4f5d1727e2b693b8f1c9a1492b5dd87008c86ea286bee1589cb458e6235fc ;;
  Darwin-x86_64) asset=tailwindcss-macos-x64; sum=a63702686a3ed048b3135d84a73be49ea9d47f880c9b20e54b3ef60934ad8cdf ;;
  MINGW* | MSYS* | CYGWIN*) asset=tailwindcss-windows-x64.exe; sum=2803583268c53f4713e89943e211081fddb186c0a4a7d07b5c342c6ae853ec95 ;;
  *) echo "no pinned Tailwind CLI for $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac

tool="bin/tailwindcss-$tailwind_version-$asset"
if [ ! -f "$tool" ]; then
  mkdir -p bin
  curl -fsSL -o "$tool.part" "https://github.com/tailwindlabs/tailwindcss/releases/download/$tailwind_version/$asset"
  mv "$tool.part" "$tool"
fi
if [ "$(sha256sum "$tool" | cut -d' ' -f1)" != "$sum" ]; then
  echo "the Tailwind CLI in $tool does not match the pinned checksum; delete it and run again" >&2
  exit 1
fi
chmod +x "$tool"

go run "github.com/a-h/templ/cmd/templ@$templ_version" generate -path internal/panel/ui
"$tool" -i internal/panel/ui/styles/input.css -o internal/panel/assets/app.css --minify

if [ "${1:-}" = "--check" ]; then
  if ! git diff --exit-code -- internal/panel/ui internal/panel/assets/app.css; then
    echo "generated panel files are out of date; run task panel-assets and commit the result" >&2
    exit 1
  fi
  untracked="$(git ls-files --others --exclude-standard -- internal/panel/ui)"
  if [ -n "$untracked" ]; then
    echo "generated panel files are not committed: $untracked" >&2
    exit 1
  fi
fi
