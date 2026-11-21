#!/usr/bin/env bash
# Writes the third-party notices of a built failover-agent: the licence and notice
# files of every module linked into the binary, found with `go version -m` in the
# module cache, and the Go licence for the standard library and runtime. A linked
# module without a licence file fails the run, so a new dependency cannot ship
# without its notice.
#
# Usage: scripts/third-party-notices.sh BINARY [OUTPUT]
# OUTPUT defaults to THIRD_PARTY_NOTICES.md.
set -euo pipefail
shopt -s nullglob nocaseglob

bin=${1:?usage: third-party-notices.sh BINARY [OUTPUT]}
out=${2:-THIRD_PARTY_NOTICES.md}
modcache=$(go env GOMODCACHE)
goroot=$(go env GOROOT)

die() {
	echo "third-party-notices: $*" >&2
	exit 1
}

# The module cache writes a capital letter as ! and its lower case.
escape() {
	local s=$1 i c
	for ((i = 0; i < ${#s}; i++)); do
		c=${s:i:1}
		if [[ $c == [[:upper:]] ]]; then printf '!%s' "${c,}"; else printf '%s' "$c"; fi
	done
}

# A heading, then each file in a fenced block under its name.
section() {
	local f
	printf '\n## %s\n' "$1"
	shift
	for f; do
		[[ -r $f ]] || die "cannot read $f"
		printf '\n### %s\n\n~~~text\n%s\n~~~\n' "${f##*/}" "$(<"$f")"
	done
}

# "path version" for each linked module; a replacement (=>) stands in for the dep above it.
modules=$(go version -m "$bin" | awk '
	BEGIN { FS = "\t" }
	$2 == "dep" { if (m) print m; m = $3 " " $4 }
	$2 == "=>" { m = $3 " " $4 }
	END { if (m) print m }') || die "$bin is not a Go binary with module information"
gover=$(go version "$bin" | awk '{ print $NF }')

trap 'rm -f "$out.tmp"' EXIT
{
	printf '# Third-party notices\n\nfailover-agent includes the software below; each section holds the licence and notice files it is distributed with.\n'
	section "Go standard library and runtime ($gover)" "$goroot/LICENSE"
	while read -r path version; do
		[[ -n $path ]] || continue
		dir=$modcache/$(escape "$path")@$(escape "$version")
		[[ -d $dir ]] || die "$path $version is not in the module cache: run go mod download"
		licences=() notices=()
		for f in "$dir"/{LICEN[CS]E,COPYING}*; do
			if [[ -f $f ]]; then licences+=("$f"); fi
		done
		for f in "$dir"/NOTICE*; do
			if [[ -f $f ]]; then notices+=("$f"); fi
		done
		((${#licences[@]})) || die "$path $version has no LICENSE or COPYING file in $dir"
		section "$path $version" "${licences[@]}" "${notices[@]}"
	done <<<"$modules"
} >"$out.tmp"
mv "$out.tmp" "$out"
