#!/usr/bin/env bash
# Builds an AppImage of llm-memory-gui from an already-built Linux binary.
#
# Usage: scripts/build-appimage.sh <binary> <version> <output.AppImage>
#
# Requires appimagetool in PATH, or APPIMAGETOOL=/path/to/appimagetool.
set -euo pipefail

if [ $# -ne 3 ]; then
	echo "usage: $0 <binary> <version> <output.AppImage>" >&2
	exit 2
fi

bin=$1
version=$2
out=$3
root=$(cd "$(dirname "$0")/.." && pwd)
app_id=io.github.morditux.llm-memory
arch=${ARCH:-x86_64}
tool=${APPIMAGETOOL:-$(command -v appimagetool || true)}
if [ -z "$tool" ]; then
	echo "appimagetool not found (set APPIMAGETOOL)" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
appdir=$work/AppDir

install -Dm755 "$bin" "$appdir/usr/bin/llm-memory-gui"
install -Dm644 "$root/gui/Icon.png" "$appdir/usr/share/icons/hicolor/512x512/apps/$app_id.png"
cp "$root/gui/Icon.png" "$appdir/$app_id.png"
cp "$root/gui/Icon.png" "$appdir/.DirIcon"

cat >"$appdir/$app_id.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=llm-memory
Comment=Desktop explorer for the llm-memory shared memory
Exec=llm-memory-gui
Icon=$app_id
Categories=Development;Utility;
Terminal=false
X-AppImage-Version=$version
DESKTOP
install -Dm644 "$appdir/$app_id.desktop" "$appdir/usr/share/applications/$app_id.desktop"

cat >"$appdir/AppRun" <<'APPRUN'
#!/bin/sh
here=$(dirname "$(readlink -f "$0")")
exec "$here/usr/bin/llm-memory-gui" "$@"
APPRUN
chmod 755 "$appdir/AppRun"

mkdir -p "$(dirname "$out")"
# Extract-and-run avoids needing FUSE (absent on CI runners).
APPIMAGE_EXTRACT_AND_RUN=1 ARCH=$arch VERSION=$version "$tool" --no-appstream "$appdir" "$out"
