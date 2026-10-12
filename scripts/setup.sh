#!/usr/bin/env bash
# Workstation tools only. This script does not create or reset a VM host.
set -euo pipefail
cd "$(dirname "$0")/.."
setup_os=$(uname -s)
setup_arch=$(uname -m)
case "$setup_os" in
  Darwin) go_os=darwin ;;
  Linux) go_os=linux ;;
  *) echo 'Use macOS, Linux, or a Linux shell in WSL.' >&2; exit 1 ;;
esac
case "$setup_arch" in
  x86_64|amd64) go_arch=amd64; aws_arch=x86_64 ;;
  arm64|aarch64) go_arch=arm64; aws_arch=aarch64 ;;
  *) echo 'Use an x86-64 or ARM64 machine.' >&2; exit 1 ;;
esac

export PATH="$HOME/.local/bin:$HOME/.pulumi/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
mkdir -p "$HOME/.local/bin" "$HOME/.local/share/saild-lite"
setup_tmp=$(mktemp -d)
trap 'rm -rf "$setup_tmp"' EXIT
have() { command -v "$1" >/dev/null 2>&1; }
fetch() { curl --proto '=https' --tlsv1.2 -fLsS --retry 3 "$1" -o "$2"; }
root() {
  if [[ $(id -u) == 0 ]]; then "$@"; else sudo "$@"; fi
}

if [[ "$setup_os" == Darwin ]]; then
  # Homebrew can exist outside PATH in a new terminal.
  if ! have brew; then
    for setup_brew in /opt/homebrew/bin/brew /usr/local/bin/brew; do
      if [[ -x "$setup_brew" ]]; then eval "$("$setup_brew" shellenv)"; break; fi
    done
  fi
  if ! have brew; then
    fetch https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh "$setup_tmp/brew.sh"
    bash "$setup_tmp/brew.sh"
    if [[ "$go_arch" == arm64 ]]; then
      eval "$(/opt/homebrew/bin/brew shellenv)"
    else
      eval "$(/usr/local/bin/brew shellenv)"
    fi
  fi
  xcode-select -p >/dev/null || { echo 'Install Xcode command line tools, then run setup again.' >&2; exit 1; }
  for setup_formula in git python3 curl openssh; do
    setup_command=$setup_formula
    [[ "$setup_formula" == openssh ]] && setup_command=ssh
    have "$setup_command" || brew install "$setup_formula"
  done
else
  setup_missing=false
  for setup_command in git python3 curl ssh ssh-keygen cc unzip tar gzip; do
    have "$setup_command" || setup_missing=true
  done
  if [[ "$setup_missing" == true ]]; then
    if have apt-get; then
      root apt-get update
      root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        git python3 curl ca-certificates openssh-client build-essential unzip tar gzip
    elif have dnf; then
      root dnf install -y git python3 curl ca-certificates openssh-clients gcc glibc-devel unzip tar gzip
    else
      echo 'Automatic system package installation requires apt-get or dnf.' >&2
      exit 1
    fi
  fi
fi

# Keep a suitable Go installation. Otherwise install an official archive and
# verify its SHA-256 against the Go release index before extracting it.
setup_go_version=$(go version 2>/dev/null || true)
if ! python3 - "$setup_go_version" <<'PY'
import pathlib, re, sys
required = tuple(map(int, re.search(r'^go ([\d.]+)', pathlib.Path('go.mod').read_text(), re.M)[1].split('.')))
found = re.search(r'go(\d+)\.(\d+)(?:\.(\d+))?(?:\s|$)', sys.argv[1])
sys.exit(0 if found and tuple(int(x or 0) for x in found.groups()) >= required else 1)
PY
then
  fetch 'https://go.dev/dl/?mode=json' "$setup_tmp/go.json"
  python3 - "$setup_tmp" "$go_os" "$go_arch" <<'PY'
import hashlib, json, pathlib, sys, urllib.request
root = pathlib.Path(sys.argv[1])
for release in json.loads((root / 'go.json').read_text()):
    if not release['stable']:
        continue
    match = next((f for f in release['files'] if f['os'] == sys.argv[2]
                  and f['arch'] == sys.argv[3] and f['kind'] == 'archive'), None)
    if match:
        break
else:
    raise SystemExit('No supported Go archive in the release index')
archive = root / 'go.tar.gz'
with urllib.request.urlopen('https://go.dev/dl/' + match['filename'], timeout=120) as src, archive.open('wb') as dst:
    digest = hashlib.sha256()
    while True:
        chunk = src.read(1024 * 1024)
        if not chunk:
            break
        digest.update(chunk)
        dst.write(chunk)
if digest.hexdigest() != match['sha256']:
    raise SystemExit('Go archive checksum does not match')
(root / 'go-version').write_text(release['version'])
PY
  setup_go_dir="$HOME/.local/share/saild-lite/$(cat "$setup_tmp/go-version")"
  mkdir -p "$setup_go_dir"
  tar -xzf "$setup_tmp/go.tar.gz" --strip-components=1 -C "$setup_go_dir"
  ln -sfn "$setup_go_dir/bin/go" "$HOME/.local/bin/go"
  ln -sfn "$setup_go_dir/bin/gofmt" "$HOME/.local/bin/gofmt"
  hash -r
fi

if ! have just; then
  fetch https://just.systems/install.sh "$setup_tmp/just.sh"
  bash "$setup_tmp/just.sh" --to "$HOME/.local/bin"
fi
if ! aws --version 2>&1 | grep -q '^aws-cli/2\.'; then
  if [[ "$setup_os" == Darwin ]]; then
    brew install awscli
  else
    fetch "https://awscli.amazonaws.com/awscli-exe-linux-$aws_arch.zip" "$setup_tmp/aws.zip"
    unzip -q "$setup_tmp/aws.zip" -d "$setup_tmp"
    "$setup_tmp/aws/install" --install-dir "$HOME/.local/share/saild-lite/aws-cli" \
      --bin-dir "$HOME/.local/bin" --update
  fi
fi
if ! have pulumi; then
  fetch https://get.pulumi.com "$setup_tmp/pulumi.sh"
  bash "$setup_tmp/pulumi.sh" --install-root "$HOME/.local/share/saild-lite/pulumi" --no-edit-path
  ln -sfn "$HOME/.local/share/saild-lite/pulumi/bin/pulumi" "$HOME/.local/bin/pulumi"
fi

# Install in a consistent location, independent of an existing GOBIN setting.
hash -r
GOBIN="$HOME/.local/bin" go install ./cmd/sail-lite
setup_path='export PATH="$HOME/.local/bin:$HOME/.pulumi/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"'
for setup_profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.bash_profile" "${ZDOTDIR:-$HOME}/.zshrc"; do
  touch "$setup_profile"
  if ! grep -Fqx "$setup_path" "$setup_profile"; then
    printf '\n# saild-lite tools\n%s\n' "$setup_path" >> "$setup_profile"
  fi
done
for setup_command in go python3 ssh ssh-keygen just aws pulumi sail-lite; do
  command -v "$setup_command"
done
printf '\nSetup complete. Run this command in your current terminal:\n%s\n' "$setup_path"
printf '\nBefore provisioning, configure AWS access and run: pulumi login\n'
