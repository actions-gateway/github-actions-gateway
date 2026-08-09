#!/usr/bin/env bash
# check-tools.sh — verify the CLI tools this project needs are installed and on
# PATH, and tell you how to fix any that are not.
#
# For each tool that does not resolve on PATH, it reports whether the tool is:
#   * installed but not on PATH  -> the exact dir to add, and where to add it
#   * not installed              -> an install command for your OS (+ docs URL)
#
# Tiers (checked in order; only a missing REQUIRED tool fails the command):
#   required  — the fast dev loop: `make check` (gofmt/lint/shellcheck/unit)
#   e2e       — local cluster + image builds: `make e2e-up`
#   extended  — heavier gates and optional workflows (security scans, dogfood)
#
# Tools built by `make tools` into .build/ (golangci-lint, cosign, ginkgo,
# controller-gen, …) are intentionally NOT checked here — they are not expected
# on PATH.
#
# Usage:
#   scripts/ci/check-tools.sh              # report all tiers; exit nonzero if a
#                                       # REQUIRED tool is missing
#   scripts/ci/check-tools.sh --required   # check only the required tier
#   scripts/ci/check-tools.sh --fix        # offer to run the install command for
#                                       # each missing tool (interactive)
#   scripts/ci/check-tools.sh go kubectl   # check only the named tools
#
# Exit status: number of missing REQUIRED tools (0 = the dev loop is ready).
set -euo pipefail
shopt -s inherit_errexit

# --- Tool registry ----------------------------------------------------------
# This registry IS the project's approved set of host CLI dependencies. Keep it
# authoritative:
#   * Adding a tool is a project decision — surface the need to maintainers
#     first; do NOT silently install a new host dependency to unblock a task.
#     Once agreed, add a row here (and to CONTRIBUTING's prerequisites list when
#     it belongs in the `required` tier) so every contributor learns to install
#     it and `make doctor` validates it.
#   * Go build-/codegen-time tools do NOT belong here — pin them in the vendored
#     tools/ module (tools/tools.go, built by `make tools`) instead.
#
# One line per tool:  name | tier | brew pkg | apt pkg | docs url | custom cmd
#   brew pkg / apt pkg : package name for `brew install` / `apt-get install`;
#                        empty when that manager can't cleanly provide it (the
#                        docs url is then the fallback).
#   custom cmd         : an exact install command that overrides brew/apt (e.g.
#                        a gcloud component). Empty for the common case.
# Cross-platform by construction: brew covers macOS, apt covers Debian/Ubuntu
# and containers, and every tool carries a docs url for everything else.
tools_registry() {
  cat <<'EOF'
go|required|go||https://go.dev/dl/|
make|required|make|make|https://www.gnu.org/software/make/|
git|required|git|git|https://git-scm.com/downloads|
gh|required|gh|gh|https://cli.github.com/|
jq|required|jq|jq|https://jqlang.github.io/jq/download/|
shellcheck|required|shellcheck|shellcheck|https://github.com/koalaman/shellcheck#installing|
docker|e2e|||https://docs.docker.com/get-docker/|
kind|e2e|kind||https://kind.sigs.k8s.io/docs/user/quick-start/#installation|
kubectl|e2e|kubernetes-cli||https://kubernetes.io/docs/tasks/tools/|
helm|e2e|helm||https://helm.sh/docs/intro/install/|
yamllint|extended|yamllint|yamllint|https://yamllint.readthedocs.io/en/stable/quickstart.html|
kubeconform|extended|kubeconform||https://github.com/yannh/kubeconform#installation|
trivy|extended|trivy||https://trivy.dev/latest/getting-started/installation/|
polaris|extended|FairwindsOps/tap/polaris||https://polaris.docs.fairwinds.com/infrastructure-as-code/#installation|
python3|extended|python|python3|https://www.python.org/downloads/|
clang|extended||clang|https://clang.llvm.org/get_started.html (macOS: xcode-select --install)|
gcloud|extended|||https://cloud.google.com/sdk/docs/install|
gke-gcloud-auth-plugin|extended|||https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl|gcloud components install gke-gcloud-auth-plugin
EOF
}

# Dirs where a tool is commonly installed but frequently NOT on PATH. Used only
# to turn a bare "not installed" into the more useful "installed here, add this
# dir to your PATH". Covers the cases this project has actually hit (macOS
# Docker Desktop ships kubectl here; the Google Cloud SDK is a self-contained
# unpack). Existence-gated, so absent dirs are simply skipped.
offpath_dirs=(
  /Applications/Docker.app/Contents/Resources/bin
  "$HOME/google-cloud-sdk/bin"
  "$HOME/.local/google-cloud-sdk/bin"
  /usr/local/go/bin
  "$HOME/go/bin"
  /opt/homebrew/bin
  /usr/local/bin
  "$HOME/.local/bin"
)

# --- Options ----------------------------------------------------------------
fix=false
declare -a tiers=(required e2e extended)
declare -a wanted=()

while (( $# )); do
  case "$1" in
    --fix)       fix=true ;;
    --required)  tiers=(required) ;;
    -h|--help)   sed -n '2,29p' "$0"; exit 0 ;;
    -*)          printf 'unknown option: %s\n' "$1" >&2; exit 2 ;;
    *)           wanted+=("$1") ;;
  esac
  shift
done

# --- Colors (only when stdout is a TTY) -------------------------------------
if [[ -t 1 ]]; then
  red=$'\033[31m'; grn=$'\033[32m'; ylw=$'\033[33m'; dim=$'\033[2m'; bold=$'\033[1m'; rst=$'\033[0m'
else
  red=''; grn=''; ylw=''; dim=''; bold=''; rst=''
fi

# in_list NEEDLE ELEM... — true if NEEDLE equals one of the ELEMs.
in_list() {
  local needle="$1"; shift
  local e
  for e in "$@"; do [[ "$e" == "$needle" ]] && return 0; done
  return 1
}

# recommend_install BREW APT URL CUSTOM — print the best install command for
# this host, or "see URL" when no package manager can cleanly provide it.
recommend_install() {
  local brew="$1" apt="$2" url="$3" custom="$4"
  if [[ -n "$custom" ]]; then printf '%s' "$custom"; return; fi
  if [[ -n "$brew" ]] && command -v brew >/dev/null 2>&1; then printf 'brew install %s' "$brew"; return; fi
  if [[ -n "$apt" ]] && command -v apt-get >/dev/null 2>&1; then printf 'sudo apt-get install -y %s' "$apt"; return; fi
  printf 'see %s' "$url"
}

# find_offpath NAME — print the first offpath dir containing an executable NAME.
find_offpath() {
  local name="$1" d
  for d in "${offpath_dirs[@]}"; do
    [[ -x "$d/$name" ]] && { printf '%s' "$d"; return 0; }
  done
  return 1
}

# profile_file — best-guess shell profile to add a PATH export to.
profile_file() {
  case "$(basename "${SHELL:-}")" in
    zsh)  printf '%s/.zshenv' "$HOME" ;;
    bash) printf '%s/.bashrc' "$HOME" ;;
    *)    printf '%s/.profile' "$HOME" ;;
  esac
}

# --- Main -------------------------------------------------------------------
missing_required=0
current_tier=''

while IFS='|' read -r name tier brew apt url custom; do
  [[ -z "$name" ]] && continue
  in_list "$tier" "${tiers[@]}" || continue
  if (( ${#wanted[@]} )) && ! in_list "$name" "${wanted[@]}"; then continue; fi

  if [[ "$tier" != "$current_tier" ]]; then
    current_tier="$tier"
    printf '\n%s%s tools%s\n' "$bold" "$tier" "$rst"
  fi

  if command -v "$name" >/dev/null 2>&1; then
    printf '  %sOK%s   %-24s %s%s%s\n' "$grn" "$rst" "$name" "$dim" "$(command -v "$name")" "$rst"
    continue
  fi

  # Not on PATH. Distinguish "installed but off PATH" from "not installed".
  if dir="$(find_offpath "$name")"; then
    printf '  %sPATH%s %-24s installed at %s but not on PATH\n' "$ylw" "$rst" "$name" "$dir"
    printf "       %sadd it:%s export PATH=\"%s:\$PATH\"   %s(in %s)%s\n" \
      "$bold" "$rst" "$dir" "$dim" "$(profile_file)" "$rst"
  else
    local_cmd="$(recommend_install "$brew" "$apt" "$url" "$custom")"
    printf '  %sMISS%s %-24s not installed\n' "$red" "$rst" "$name"
    printf '       %sinstall:%s %s\n' "$bold" "$rst" "$local_cmd"
    [[ -n "$url" && "$local_cmd" != "see "* ]] && printf '       %sdocs: %s%s\n' "$dim" "$url" "$rst"
    if $fix && [[ "$local_cmd" != "see "* ]] && [[ -e /dev/tty ]]; then
      read -r -p "       run it now? [y/N] " reply < /dev/tty || reply=''
      if [[ "$reply" == [yY]* ]]; then
        eval "$local_cmd" || printf '       %sinstall failed%s\n' "$red" "$rst"
      fi
    fi
  fi

  if command -v "$name" >/dev/null 2>&1; then continue; fi
  [[ "$tier" == required ]] && missing_required=$((missing_required + 1))
done < <(tools_registry)

echo
if (( missing_required == 0 )); then
  printf '%sRequired toolchain is ready.%s\n' "$grn" "$rst"
else
  printf '%s%d required tool(s) missing — the dev loop will not work until they are installed.%s\n' \
    "$red" "$missing_required" "$rst"
  printf 'After a PATH change, restart your shell (or re-source the profile) and re-run this script.\n'
fi
exit "$missing_required"
