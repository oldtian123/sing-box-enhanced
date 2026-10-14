#!/usr/bin/env bash
set -euo pipefail

# Root-only headless deployment; never runs the desktop package's maintscripts.
fail() { printf '%s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || fail 'Run this installer as root.'
source /etc/os-release
[[ ${ID:-} == debian && ${VERSION_ID:-} == 13 && ${VERSION_CODENAME:-} == trixie ]] || fail 'Debian 13 (trixie) is required.'
architecture=$(dpkg --print-architecture)
[[ $architecture == amd64 || $architecture == arm64 ]] || fail 'Only amd64 and arm64 are supported.'
[[ $# -le 1 ]] || fail "Usage: $0 [proxy-port]"
port=${1:-${WARP_PORT:-40000}}
[[ $port =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || fail 'Invalid WARP proxy port.'
port=$((10#$port))
root_dir=${SBE_ROOT:-/etc/sing-box-enhanced}
[[ $root_dir =~ ^/[A-Za-z0-9_./-]+$ && ! -L $root_dir ]] || fail 'SBE_ROOT must be an absolute, plain directory path.'
root_dir=$(realpath -m -- "$root_dir")
[[ $root_dir != / ]] || fail 'SBE_ROOT cannot be the filesystem root.'
unit_file=/etc/systemd/system/warp-svc.service
for path in /var/lib/cloudflare-warp /run/cloudflare-warp "$root_dir/warp" "$unit_file" /lib/systemd/system/warp-svc.service /usr/lib/systemd/system/warp-svc.service; do
    [[ ! -e $path && ! -L $path ]] || fail 'Existing WARP state, binaries or service require review; nothing was replaced.'
done
! command -v warp-cli >/dev/null 2>&1 || fail 'Existing WARP installation was left unchanged; review its mode explicitly.'
[[ -z $(dpkg --audit) ]] || fail 'Unfinished package operations require review before installing dependencies.'
temporary_dir=$(mktemp -d)
trap 'rm -rf -- "$temporary_dir"' EXIT
apt_auth=(-o APT::Get::AllowUnauthenticated=false -o Acquire::AllowInsecureRepositories=false -o Acquire::AllowDowngradeToInsecureRepositories=false)
apt_options=("${apt_auth[@]}" -o APT::Sandbox::User=root -o "Dir::Cache::archives=$temporary_dir/deps" -y --no-install-recommends --no-upgrade)
mkdir -p "$temporary_dir/deps/partial"
shopt -s nullglob
reviewed_install() {
    apt-get "${apt_options[@]}" --download-only install "$@"
    for package in "$temporary_dir/deps/"*.deb; do
        rm -rf -- "$temporary_dir/control"
        dpkg-deb -e "$package" "$temporary_dir/control"
        if grep -R -E 'adduser|useradd|addgroup|groupadd|newusers|usermod|groupmod|chpasswd|systemd-sysusers|sysusers[.]d|/etc/(passwd|shadow|group|gshadow)' "$temporary_dir/control" >/dev/null; then
            fail "Dependency maintscripts may change users/groups; refusing $(basename "$package")."
        fi
        if dpkg-deb -c "$package" | grep -E '/sysusers[.]d/' >/dev/null; then
            fail "Dependency ships sysusers configuration; refusing $(basename "$package")."
        fi
    done
    # Installation only uses the packages from the reviewed private cache.
    apt-get "${apt_options[@]}" --no-download install "$@"
}
keyring=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
source_list=/etc/apt/sources.list.d/cloudflare-client.list
if [[ -e $keyring || -L $keyring || -e $source_list || -L $source_list ]]; then
    [[ -f $keyring && ! -L $keyring && -f $source_list && ! -L $source_list ]] || fail 'Existing repository files require review.'
    grep -Eq '^deb +\[[^]]*signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring[.]gpg[^]]*\] +https://pkg[.]cloudflareclient[.]com/ +trixie +main *$' "$source_list" || fail 'Existing repository configuration requires review; no files were overwritten.'
else
    command -v curl >/dev/null || fail 'curl is required to configure the signed repository.'
    if ! command -v gpg >/dev/null; then
        apt-get "${apt_auth[@]}" update
        reviewed_install gpg
    fi
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 60 -fsSL https://pkg.cloudflareclient.com/pubkey.gpg -o "$temporary_dir/pubkey.gpg"
    gpg --batch --dearmor --output "$temporary_dir/keyring.gpg" "$temporary_dir/pubkey.gpg"
    install -m 0644 "$temporary_dir/keyring.gpg" "$keyring"
    printf 'deb [arch=%s signed-by=%s] https://pkg.cloudflareclient.com/ trixie main\n' "$architecture" "$keyring" > "$source_list"
fi
apt-get "${apt_auth[@]}" update
(cd "$temporary_dir" && apt-get "${apt_auth[@]}" -o APT::Sandbox::User=root download cloudflare-warp)
# Runtime dependencies are downloaded only; their maintscripts never execute.
mkdir -p "$temporary_dir/libs/partial" "$temporary_dir/payload" "$temporary_dir/private/lib" "$temporary_dir/private/licenses"
apt-get "${apt_options[@]}" -o "Dir::Cache::archives=$temporary_dir/libs" --download-only install libnss3 libtss2-esys-3.0.2-0t64 libtss2-tctildr0t64
[[ $architecture == amd64 ]] && multiarch=x86_64-linux-gnu || multiarch=aarch64-linux-gnu
package_names=()
for package in "$temporary_dir/libs/"*.deb; do
    package_names+=("$(dpkg-deb -f "$package" Package)")
    dpkg-deb --fsys-tarfile "$package" > "$temporary_dir/payload.tar"
    tar -tf "$temporary_dir/payload.tar" > "$temporary_dir/members"
    grep -E "^\./usr/lib/$multiarch/lib[^/]*[.]so[^/]*$|^\./usr/lib/$multiarch/lib(freebl3|freeblpriv3|nssdbm3|softokn3)[.]chk$|^\./lib/$multiarch/[^/]+$|^\./usr/share/doc/[^/]+/copyright$" "$temporary_dir/members" > "$temporary_dir/selected" || [[ $? -eq 1 ]]
    tar -xf "$temporary_dir/payload.tar" -C "$temporary_dir/payload" -T "$temporary_dir/selected"
done
for library in "$temporary_dir/payload/usr/lib/$multiarch/"lib*.so* "$temporary_dir/payload/usr/lib/$multiarch/"lib*.chk "$temporary_dir/payload/lib/$multiarch/"*; do
    destination=$temporary_dir/private/lib/$(basename "$library")
    [[ ! -e $destination && ! -L $destination ]] || fail 'Conflicting private runtime libraries require review.'
    [[ $library != *.chk || ! -L $library ]] || fail 'NSS checksum files must be ordinary files.'
    if [[ -L $library ]]; then
        target=$(readlink "$library")
        [[ -n $target && $target != */* && $target != . && $target != .. ]] || fail 'Library symlink must point within its own directory.'
        ln -s "$target" "$destination"
    else
        [[ -f $library ]] || fail 'Only ordinary runtime library files are allowed.'
        install -m 0644 "$library" "$destination"
    fi
done
for library in "$temporary_dir/private/lib/"*; do [[ -f $library ]] || fail 'Private runtime library symlink does not resolve.'; done
for name in "${package_names[@]}"; do
    license=$(realpath -e -- "$temporary_dir/payload/usr/share/doc/$name/copyright")
    [[ $license == "$temporary_dir/payload/usr/share/doc/"* && -f $license ]] || fail 'Package copyright must resolve inside its private payload.'
    install -m 0644 "$license" "$temporary_dir/private/licenses/$name.copyright"
done
warp_packages=("$temporary_dir/"cloudflare-warp_*.deb)
[[ ${#warp_packages[@]} -eq 1 ]] || fail 'Expected exactly one authenticated Cloudflare package.'
dpkg-deb --fsys-tarfile "${warp_packages[0]}" > "$temporary_dir/warp.tar"
tar -tf "$temporary_dir/warp.tar" > "$temporary_dir/warp-members"
warp_files=(./bin/warp-cli ./bin/warp-svc)
notices=./usr/lib/warp/data/flutter_assets/NOTICES.Z
if grep -Fxq "$notices" "$temporary_dir/warp-members"; then warp_files+=("$notices"); fi
tar -xf "$temporary_dir/warp.tar" -C "$temporary_dir" "${warp_files[@]}"
if [[ ${#warp_files[@]} -eq 3 ]]; then
    [[ -f $temporary_dir/$notices && ! -L $temporary_dir/$notices ]] || fail 'Cloudflare notices must be an ordinary file.'
    install -m 0644 "$temporary_dir/$notices" "$temporary_dir/private/licenses/cloudflare-warp.NOTICES.Z"
fi
install -d -m 0700 "$root_dir" "$root_dir/warp" "$root_dir/warp/bin" "$root_dir/warp/lib" "$root_dir/warp/licenses" "$root_dir/bin"
install -m 0755 "$temporary_dir/bin/warp-cli" "$temporary_dir/bin/warp-svc" "$root_dir/warp/bin/"
cp -a "$temporary_dir/private/lib/." "$root_dir/warp/lib/"
cp -a "$temporary_dir/private/licenses/." "$root_dir/warp/licenses/"
printf '%s\n' 'https://developers.cloudflare.com/warp-client/legal/3rdparty/' > "$root_dir/warp/licenses/cloudflare-warp.sources.txt"
cli=$root_dir/warp/bin/warp-cli
ln -s "$cli" "$root_dir/bin/warp-cli"
cat > "$unit_file" <<EOF
[Unit]
Description=Cloudflare Zero Trust Client Daemon (headless)
After=pre-network.target
[Service]
Type=simple
ExecStart=$root_dir/warp/bin/warp-svc
Environment=LD_LIBRARY_PATH=$root_dir/warp/lib
User=root
DynamicUser=no
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_PTRACE CAP_DAC_OVERRIDE CAP_NET_RAW CAP_SETUID CAP_SETGID
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_PTRACE CAP_DAC_OVERRIDE CAP_NET_RAW CAP_SETUID CAP_SETGID
StateDirectory=cloudflare-warp
RuntimeDirectory=cloudflare-warp
LogsDirectory=cloudflare-warp
Restart=always
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now warp-svc.service
if "$cli" mode proxy </dev/null >/dev/null 2>&1 && "$cli" proxy port "$port" </dev/null >/dev/null 2>&1 && "$cli" tunnel protocol set MASQUE </dev/null >/dev/null 2>&1; then
    printf 'Headless local proxy configured on 127.0.0.1:%s. Nothing was connected.\n' "$port"
else
    printf 'Headless daemon installed; proxy setup is pending. No terms were accepted or connection requested.\n' >&2
    printf 'Review terms yourself with %s registration new, then configure mode proxy, proxy port %s and tunnel protocol set MASQUE.\n' "$cli" "$port" >&2
    exit 2
fi
printf 'Registration and terms review remain manual. Use sb to connect after checking the proxy settings.\n'
