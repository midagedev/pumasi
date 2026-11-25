#!/bin/sh
# Served by a pumasi controller. Fetches the controller's build for this
# machine and joins this host to it. Everything it puts here:
#   ~/.local/bin/pumasi, ~/.cache/pumasi/, and a service that runs the agent.
set -eu

main() {
  controller='{{controller}}'
  pin='{{pin}}'
  if [ $# -lt 1 ]; then
    echo "usage: ... | sh -s -- <token> [--name NAME] [--cpus N] [--mem 6G] [--desktop]" >&2
    exit 2
  fi
  token=$1
  shift
  case "$(uname -s)/$(uname -m)" in
    Linux/x86_64) arch=amd64 ;;
    Linux/aarch64 | Linux/arm64) arch=arm64 ;;
    *)
      echo "pumasi: this is $(uname -s)/$(uname -m); only Linux on x86_64 or arm64 can join" >&2
      exit 1
      ;;
  esac
  bin=$HOME/.local/bin/pumasi
  mkdir -p "$HOME/.local/bin"
  get() { curl -fsSk --pinnedpubkey "$pin" "https://$controller$1"; }
  get "/dl/pumasi-linux-$arch" > "$bin.new"
  want=$(get "/dl/pumasi-linux-$arch.sha256")
  if ! echo "$want  $bin.new" | sha256sum -c - > /dev/null 2>&1; then
    rm -f "$bin.new"
    echo "pumasi: the download does not match its checksum" >&2
    exit 1
  fi
  chmod +x "$bin.new"
  mv "$bin.new" "$bin"
  exec "$bin" join "$@" "$controller" "$pin" "$token"
}

main "$@"
