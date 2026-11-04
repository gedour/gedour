#!/usr/bin/env bash
# Fails a release whose tag differs from the image tag pinned in deploy/k8s/base and the README's overlay
# example (`?ref=` and `newTag:`). Pre-release tags (vX.Y.Z-*) are not pinned and pass.
# Usage: check-release-pin.sh <tag> [repo root] | check-release-pin.sh --self-test
set -euo pipefail

readonly BASE_FILE="deploy/k8s/base/kustomization.yaml"
readonly README_FILE="README.md"
# Strict semver: punctuation after a pin in prose (a period, a quote, a parenthesis) is not part of it.
readonly REF='\?ref=v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?'
readonly TAG_LINE='^[[:space:]]*newTag:[[:space:]]*v[0-9]'

# check TAG ROOT: prints every mismatch, returns 1 on any.
check() {
  local tag="$1" root="$2" bad=0 pins
  if [[ ! "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]]; then
    echo "not a release tag: ${tag}" >&2
    return 2
  fi
  if [[ "${tag}" == *-* ]]; then
    echo "${tag} is a pre-release: pins not checked"
    return 0
  fi
  # Anchored to real pins (a v-version value) so prose mentioning `newTag:` or `?ref=` never counts.
  pins="$(grep -E "${TAG_LINE}" "${root}/${BASE_FILE}" | awk '{ print $2 }')" || pins=""
  if [[ "${pins}" != "${tag}" ]]; then
    echo "${BASE_FILE}: newTag is '${pins//$'\n'/, }', want ${tag}" >&2
    bad=1
  fi
  pins="$({
    grep -oE "${REF}" "${root}/${README_FILE}" | sed 's/^?ref=//'
    grep -E "${TAG_LINE}" "${root}/${README_FILE}" | awk '{ print $2 }'
  })" || pins=""
  if [[ -z "${pins}" ]]; then
    echo "${README_FILE}: no ?ref= or newTag: pin found" >&2
    bad=1
  fi
  local pin
  while read -r pin; do
    if [[ -n "${pin}" && "${pin}" != "${tag}" ]]; then
      echo "${README_FILE}: pin '${pin}', want ${tag}" >&2
      bad=1
    fi
  done <<<"${pins}"
  return "${bad}"
}

self_test() {
  local failed=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT
  # fixture VERSION_BASE VERSION_REF VERSION_README_TAG: a minimal repo in ${tmp}.
  fixture() {
    mkdir -p "${tmp}/deploy/k8s/base"
    printf 'images:\n  - name: ghcr.io/gedour/gedour\n    newTag: %s\n' "$1" >"${tmp}/${BASE_FILE}"
    printf 'resources:\n  - https://github.com/gedour/gedour//deploy/k8s/base?ref=%s\n  - x//c?ref=%s\nimages:\n    newTag: %s\n' \
      "$2" "$2" "$3" >"${tmp}/${README_FILE}"
  }
  # expect WANT_CODE TAG BASE REF README_TAG NAME
  expect() {
    local code=0
    fixture "$3" "$4" "$5"
    check "$2" "${tmp}" >/dev/null 2>&1 || code=$?
    if [[ "${code}" -ne "$1" ]]; then
      echo "self-test FAIL: $6 (want exit $1, got ${code})" >&2
      failed=1
    fi
  }
  expect 0 v1.2.3 v1.2.3 v1.2.3 v1.2.3 "all pins match"
  expect 1 v1.2.4 v1.2.3 v1.2.4 v1.2.4 "base pin behind"
  expect 1 v1.2.4 v1.2.4 v1.2.3 v1.2.4 "README ?ref= behind"
  expect 1 v1.2.4 v1.2.4 v1.2.4 v1.2.3 "README newTag behind"
  expect 1 v1.2.4 v1.2.4 v1.2.4 v1.2.40 "README pin with a longer version"
  expect 0 v1.3.0-rc.1 v1.2.3 v1.2.3 v1.2.3 "pre-release skips the check"
  expect 2 1.2.3 1.2.3 1.2.3 1.2.3 "tag without v"
  # The second ?ref= alone drifts: every pin is checked, not the first.
  fixture v1.2.4 v1.2.4 v1.2.4
  sed -i 's|x//c?ref=v1.2.4|x//c?ref=v1.2.3|' "${tmp}/${README_FILE}"
  if check v1.2.4 "${tmp}" >/dev/null 2>&1; then
    echo "self-test FAIL: second ?ref= behind" >&2
    failed=1
  fi
  # Prose quoting the pin syntax is not a pin.
  fixture v1.2.4 v1.2.4 v1.2.4
  # shellcheck disable=SC2016 # literal backticks, as the README writes them
  printf 'A tag must equal the `?ref=` and `newTag:` pins; see ?ref=v1.2.4.\n' >>"${tmp}/${README_FILE}"
  if ! check v1.2.4 "${tmp}" >/dev/null 2>&1; then
    echo "self-test FAIL: prose about pins counted as a pin" >&2
    failed=1
  fi
  # The repository itself: its README and base agree with the base's own tag.
  local root repo_tag code=0
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  repo_tag="$(grep -E "${TAG_LINE}" "${root}/${BASE_FILE}" | awk '{ print $2 }')"
  check "${repo_tag}" "${root}" >/dev/null || code=$?
  if [[ "${code}" -ne 0 ]]; then
    echo "self-test FAIL: the repository's own pins disagree with ${repo_tag:-<none>} (exit ${code})" >&2
    failed=1
  fi
  [[ "${failed}" -eq 0 ]] && echo "check-release-pin self-test: OK"
  return "${failed}"
}

main() {
  case "${1:-}" in
    --self-test) self_test ;;
    "")
      echo "usage: $0 <tag> [repo root] | $0 --self-test" >&2
      exit 2
      ;;
    *)
      local root
      root="${2:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
      check "$1" "${root}"
      ;;
  esac
}

main "$@"
