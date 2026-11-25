#!/bin/sh
# Served by a pumasi controller. Fetches the controller's build for this
# machine and joins this host to it. Everything it puts here:
#   ~/.local/bin/pumasi, ~/.cache/pumasi/, and a service that runs the agent.
set -eu

main() {
  controller='{{controller}}'
  pin='{{pin}}'
  if [ -z "${PUMASI_TOKEN:-}" ]; then
    echo "usage: ... | PUMASI_TOKEN=<token> sh -s -- [--name NAME] [--cpus N] [--mem 6G] [--desktop|--server]" >&2
    echo "(\`pumasi invite\` on the controller prints the whole line)" >&2
    exit 2
  fi
  case "$(uname -s)/$(uname -m)" in
    Linux/x86_64) build=linux-amd64 ;;
    Linux/aarch64 | Linux/arm64) build=linux-arm64 ;;
    Darwin/arm64) build=darwin-arm64 ;;
    Darwin/x86_64) build=darwin-amd64 ;;
    *)
      echo "pumasi: this is $(uname -s)/$(uname -m); Linux and macOS on x86_64 or arm64 can join with this line (Windows has its own, see \`pumasi invite\`)" >&2
      exit 1
      ;;
  esac
  if command -v sha256sum > /dev/null 2>&1; then
    sum() { sha256sum "$1" | cut -d' ' -f1; }
  else
    sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
  fi
  bin=$HOME/.local/bin/pumasi
  mkdir -p "$HOME/.local/bin"
  get() { curl -fsSk --pinnedpubkey "$pin" "https://$controller$1"; }
  get "/dl/pumasi-$build" > "$bin.new"
  want=$(get "/dl/pumasi-$build.sha256")
  if [ "$(sum "$bin.new")" != "$want" ]; then
    rm -f "$bin.new"
    echo "pumasi: the download does not match its checksum" >&2
    exit 1
  fi
  chmod +x "$bin.new"
  mv "$bin.new" "$bin"
  # The token stays in the environment: an argument would show in ps.
  exec "$bin" join "$@" "$controller" "$pin"
}

main "$@"
