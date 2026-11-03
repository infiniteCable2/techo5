#!/bin/sh
# Public fallback assets only; no network, configuration or device credentials.
set -eu

source_dir=${1:-/usr/share/techo5/fonts}
destination=${2:-/data/misc/techo5/fonts}
[ -d "$source_dir" ] || exit 0
for name in zz-noto-emoji.OFL.txt zz-noto-emoji.ttf; do
	[ -f "$source_dir/$name" ] || { echo "missing bundled font asset: $name" >&2; exit 1; }
done
mkdir -p "$destination"
temporary=
trap '[ -z "$temporary" ] || rm -f "$temporary"' EXIT
for name in zz-noto-emoji.OFL.txt zz-noto-emoji.ttf; do
	# Also leave a dangling owner symlink alone.
	[ ! -e "$destination/$name" ] && [ ! -L "$destination/$name" ] || continue
	temporary=$(mktemp "$destination/.bundled-font.XXXXXX")
	cp "$source_dir/$name" "$temporary"
	chmod 644 "$temporary"
	mv -n "$temporary" "$destination/$name"
	# mv -n leaves its input if another writer has installed the destination.
	rm -f "$temporary"
	temporary=
done
