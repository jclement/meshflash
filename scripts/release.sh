#!/usr/bin/env bash
# Cut a release: pick the next version, tag it, push.
#
# Pushing the tag is the whole trigger — .github/workflows/release.yml sees a
# v* tag and runs GoReleaser. Nothing is built locally.
#
# Usage:
#   scripts/release.sh              # interactive: choose the segment to bump
#   scripts/release.sh patch        # non-interactive
#   scripts/release.sh minor
#   scripts/release.sh major
#   scripts/release.sh 1.4.0        # explicit version
#
# Env:
#   RELEASE_BRANCH  branch releases must be cut from (default: main)
#   DRY_RUN=1       do everything except create and push the tag
set -euo pipefail

cd "$(dirname "$0")/.."

BRANCH="${RELEASE_BRANCH:-main}"
DRY_RUN="${DRY_RUN:-}"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
dim() { printf '\033[2m%s\033[0m\n' "$*"; }
die() {
  printf '\033[31merror:\033[0m %s\n' "$*" >&2
  exit 1
}

# --- preflight ------------------------------------------------------------
# A tag is effectively immutable once pushed, so every check that can prevent
# a bad one is worth doing before we get there.

command -v git >/dev/null || die "git is not installed"

git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository"

current_branch="$(git symbolic-ref --quiet --short HEAD || echo DETACHED)"
[ "$current_branch" = "$BRANCH" ] ||
  die "releases are cut from '$BRANCH', but you are on '$current_branch'"

[ -z "$(git status --porcelain)" ] ||
  die "working tree is dirty; commit or stash first"

have_remote=false
if git remote get-url origin >/dev/null 2>&1; then
  dim "fetching origin..."
  # An unreachable origin is not fatal here: the repository may not exist yet,
  # or we may be offline. Skip the ahead/behind check and let the push at the
  # end be the thing that fails, with a clearer error.
  if git fetch --quiet origin --tags 2>/dev/null; then
    have_remote=true
    local_head="$(git rev-parse HEAD)"
    remote_head="$(git rev-parse "origin/$BRANCH" 2>/dev/null || echo "")"
    if [ -n "$remote_head" ] && [ "$local_head" != "$remote_head" ]; then
      if git merge-base --is-ancestor "$local_head" "$remote_head"; then
        die "$BRANCH is behind origin/$BRANCH; pull first"
      fi
      dim "note: local $BRANCH is ahead of origin; it will be pushed"
    fi
  else
    dim "warning: could not reach origin ($(git remote get-url origin))"
    dim "         tagging anyway; the push may fail if the repo does not exist yet"
  fi
else
  dim "no 'origin' remote; the tag will only be created locally"
fi

# --- work out the next version -------------------------------------------

latest="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
if [ -z "$latest" ]; then
  dim "no existing v* tags; starting from v0.0.0"
  latest="v0.0.0"
fi
current="${latest#v}"

IFS='.' read -r cur_major cur_minor cur_patch <<<"$current"
# Tolerate a tag like v1.2 or a prerelease suffix rather than failing outright.
cur_major="${cur_major:-0}"
cur_minor="${cur_minor:-0}"
cur_patch="${cur_patch%%-*}"
cur_patch="${cur_patch:-0}"

next_patch="${cur_major}.${cur_minor}.$((cur_patch + 1))"
next_minor="${cur_major}.$((cur_minor + 1)).0"
next_major="$((cur_major + 1)).0.0"

choice="${1:-}"
case "$choice" in
patch) version="$next_patch" ;;
minor) version="$next_minor" ;;
major) version="$next_major" ;;
"")
  bold "Current version: v${current}"
  echo
  echo "  1) patch   v${next_patch}   bug fixes"
  echo "  2) minor   v${next_minor}   new features, backwards compatible"
  echo "  3) major   v${next_major}   breaking changes"
  echo
  # Read from the terminal, not stdin: mise may have redirected it.
  if [ -t 0 ]; then
    read -r -p "Which segment? [1/2/3] " answer </dev/tty
  else
    die "not a terminal; pass patch, minor, major, or an explicit version"
  fi
  case "$answer" in
  1 | patch | p) version="$next_patch" ;;
  2 | minor | m) version="$next_minor" ;;
  3 | major | M) version="$next_major" ;;
  *) die "unrecognised choice '$answer'" ;;
  esac
  ;;
*)
  # An explicit version, with or without the leading v.
  version="${choice#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "'$choice' is not a semver version or one of: patch, minor, major"
  ;;
esac

tag="v${version}"

git rev-parse -q --verify "refs/tags/$tag" >/dev/null &&
  die "tag $tag already exists"

# --- confirm --------------------------------------------------------------

echo
bold "Releasing $tag"
dim "  from:   $(git rev-parse --short HEAD) on $BRANCH"

# Only diff against the previous tag when there actually is one. Under
# `set -o pipefail` a `git log v0.0.0..HEAD` against a tag that was never
# created fails the whole pipeline and, with `set -e`, exits the script
# silently — right before it would have tagged anything.
if git rev-parse -q --verify "refs/tags/${latest}" >/dev/null; then
  commits="$(git rev-list --count "${latest}..HEAD")"
  if [ "$commits" != "0" ]; then
    dim "  since $latest: $commits commits"
    echo
    git log --pretty='  %C(dim)%h%Creset %s' "${latest}..HEAD" | head -20
    [ "$commits" -gt 20 ] && dim "  ... and $((commits - 20)) more"
  fi
else
  dim "  first release"
fi
echo

if [ -n "$DRY_RUN" ]; then
  bold "DRY_RUN set — stopping before tagging."
  exit 0
fi

if [ -t 0 ]; then
  read -r -p "Tag and push? [y/N] " confirm </dev/tty
  case "$confirm" in
  y | Y | yes | YES) ;;
  *) die "cancelled" ;;
  esac
fi

# --- tag and push ---------------------------------------------------------

git tag -a "$tag" -m "Release $tag"
echo "created tag $tag"

if [ "$have_remote" = true ]; then
  git push origin "$BRANCH"
  git push origin "$tag"
  echo
  bold "Pushed $tag."
  slug="$(git remote get-url origin | sed -E 's#(git@|https://)github.com[:/]##; s#\.git$##')"
  dim "GoReleaser is now building the release:"
  dim "  https://github.com/${slug}/actions"
  dim "  https://github.com/${slug}/releases/tag/${tag}"
else
  echo
  bold "Tag $tag created locally. Add an 'origin' remote and push to release."
fi
