#!/bin/sh
# Construit le paquet Debian de kura-agent.
#   scripts/build-deb.sh VERSION ARCH BINAIRE DOSSIER_SORTIE
# VERSION : 0.7.0 (une préversion 0.7.0-rc1 devient 0.7.0~rc1) ; ARCH : amd64 | arm64.
set -eu

version=$1 arch=$2 binary=$3 out=$4
case "$arch" in amd64|arm64) ;; *) echo "architecture $arch non prise en charge" >&2; exit 2 ;; esac
debver=$(printf '%s' "$version" | sed 's/-/~/')
here=$(cd "$(dirname "$0")/.." && pwd)
root=$(mktemp -d)
chmod 0755 "$root"
trap 'rm -rf "$root"' EXIT

install -D -m 0755 "$binary" "$root/usr/bin/kura-agent"
install -D -m 0644 "$here/packaging/deb/kura-agent.sysusers" "$root/usr/lib/sysusers.d/kura-agent.conf"
install -D -m 0644 "$here/packaging/deb/copyright" "$root/usr/share/doc/kura-agent/copyright"
install -D -m 0644 "$here/README.md" "$root/usr/share/doc/kura-agent/README.md"
install -D -m 0644 "$here/configs/example-linux.yaml" "$root/usr/share/doc/kura-agent/examples/config.yaml"
install -d -m 0755 "$root/DEBIAN"
for s in postinst prerm postrm; do
	install -m 0755 "$here/packaging/deb/$s" "$root/DEBIAN/$s"
done
size=$(du -sk --exclude=DEBIAN "$root" | cut -f1)
cat > "$root/DEBIAN/control" <<CONTROL
Package: kura-agent
Version: $debver
Architecture: $arch
Maintainer: E-C-G <kura-agent@users.noreply.github.com>
Installed-Size: $size
Depends: systemd (>= 254), gpgv, ca-certificates
Section: admin
Priority: optional
Homepage: https://github.com/E-C-G/kura-agent
Description: sauvegarde planifiée vers Wasabi
 Agent Kura : sauvegarde nocturne des dossiers du poste vers un bucket Wasabi
 (aws s3 sync), avec garde-fou contre les suppressions massives et bilan vers
 healthchecks ou un webhook. Configurer le poste avec
 « sudo kura-agent install --package <fichier>.kura », préparé par la console.
CONTROL

mkdir -p "$out"
deb="$out/kura-agent_${debver}_${arch}.deb"
dpkg-deb --root-owner-group -Zxz --build "$root" "$deb" >/dev/null
echo "$deb"
