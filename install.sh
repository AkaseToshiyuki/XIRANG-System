#!/bin/sh
set -eu

PROGRAM=${0##*/}

# Ed25519 public key for the XIRANG release signing key. The matching private
# key signs dist/SHA256SUMS during the tagged release workflow. Verifying the
# manifest against a key that ships with this script is what makes a substituted
# release detectable: the archive and its checksums arrive over the same channel
# from the same GitHub Release, so the checksum alone proves only that the two
# agree with each other.
RELEASE_SIGNING_PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAP2bewUoBIEUWCEo5KNibWE9HGOY8i85URlC6tVRUlyU=
-----END PUBLIC KEY-----'
REPOSITORY=${XIRANG_GITHUB_REPOSITORY:-AkaseToshiyuki/XIRANG-System}
RELEASE_VERSION=${XIRANG_VERSION:-latest}
ARCHIVE_PATH=
EXPECTED_SHA256=
SKIP_DEPENDENCIES=0
ALLOW_UNSYNCHRONIZED_CLOCK=0
NONINTERACTIVE=0
DESTDIR_VALUE=
TEMP_DIR=
CURL_CONFIG=

usage() {
    cat <<EOF
Install a verified XIRANG native Linux release.

Usage:
  ./${PROGRAM} [options]

Options:
  --version VERSION          Install a release tag/version (default: latest)
  --repo OWNER/REPO          GitHub repository (default: ${REPOSITORY})
  --archive PATH             Install a local release archive
  --sha256 HASH              Expected SHA-256 for a local archive. Supplying it
                             establishes trust out of band and replaces the
                             release signature check.
  --skip-dependencies        Do not invoke the host package manager
  --allow-unsynchronized-clock
                             Continue when NTP cannot synchronize (not recommended)
  --non-interactive          Install files without the role setup wizard
  --destdir PATH             Stage files below PATH without root (testing/package use)
  -h, --help                 Show this help

EOF
}

log() {
    printf '%s\n' "[XIRANG] $*"
}

fail() {
    printf '%s\n' "[XIRANG] ERROR: $*" >&2
    exit 1
}

cleanup() {
    if [ -n "${TEMP_DIR}" ] && [ -d "${TEMP_DIR}" ]; then
        rm -rf "${TEMP_DIR}"
    fi
}

trap cleanup EXIT
trap 'exit 130' HUP INT TERM

need_value() {
    [ "$#" -ge 2 ] || fail "$1 requires a value"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            need_value "$@"
            RELEASE_VERSION=$2
            shift 2
            ;;
        --repo)
            need_value "$@"
            REPOSITORY=$2
            shift 2
            ;;
        --archive)
            need_value "$@"
            ARCHIVE_PATH=$2
            shift 2
            ;;
        --sha256)
            need_value "$@"
            EXPECTED_SHA256=$2
            shift 2
            ;;
        --skip-dependencies)
            SKIP_DEPENDENCIES=1
            shift
            ;;
        --allow-unsynchronized-clock)
            ALLOW_UNSYNCHRONIZED_CLOCK=1
            shift
            ;;
        --non-interactive)
            NONINTERACTIVE=1
            shift
            ;;
        --destdir)
            need_value "$@"
            DESTDIR_VALUE=$2
            NONINTERACTIVE=1
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown option: $1 (run ./${PROGRAM} --help)"
            ;;
    esac
done

case "${REPOSITORY}" in
    *[!A-Za-z0-9_.-]*/*|*/*[!A-Za-z0-9_.-]*|*/*/*|/*|*/|"")
        fail "--repo must use OWNER/REPO format"
        ;;
    */*) ;;
    *) fail "--repo must use OWNER/REPO format" ;;
esac
case "${RELEASE_VERSION}" in
    *[!A-Za-z0-9_.-]*|"")
        fail "--version contains unsupported characters"
        ;;
esac

[ "$(uname -s)" = "Linux" ] || fail "this installer supports Linux only"
case "$(uname -m)" in
    x86_64|amd64)
        ARCH=amd64
        ;;
    aarch64|arm64)
        ARCH=arm64
        ;;
    *)
        fail "unsupported CPU architecture: $(uname -m); use AMD64 or ARM64"
        ;;
esac

if [ -z "${DESTDIR_VALUE}" ]; then
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=
    else
        command -v sudo >/dev/null 2>&1 ||
            fail "sudo is required; install sudo or run this installer as root"
        log "System installation needs administrator privileges."
        log "sudo will now request your password using its normal terminal prompt."
        sudo -v
        SUDO=sudo
    fi
else
    SUDO=
    case "${DESTDIR_VALUE}" in
        /*) ;;
        *) fail "--destdir must be an absolute path" ;;
    esac
fi

run_root() {
    if [ -n "${SUDO}" ]; then
        sudo "$@"
    else
        "$@"
    fi
}

missing_runtime_commands() {
    missing=
    for command_name in tar sha256sum openssl install ping ip lspci sensors ssh sshd ssh-keygen exportfs mount.nfs; do
        if ! command -v "${command_name}" >/dev/null 2>&1; then
            missing="${missing} ${command_name}"
        fi
    done
    if [ -z "${ARCHIVE_PATH}" ]; then
        for command_name in curl jq; do
            if ! command -v "${command_name}" >/dev/null 2>&1; then
                missing="${missing} ${command_name}"
            fi
        done
    fi
    printf '%s' "${missing}"
}

install_dependencies() {
    if [ -n "${DESTDIR_VALUE}" ]; then
        log "Staging mode: host runtime dependencies are not changed."
        return
    fi
    missing=$(missing_runtime_commands)
    if [ -z "${missing}" ]; then
        log "Required host tools are already installed."
        return
    fi
    if [ "${SKIP_DEPENDENCIES}" -eq 1 ]; then
        fail "missing required commands:${missing}"
    fi
    log "Installing missing host tools:${missing}"
    if command -v apt-get >/dev/null 2>&1; then
        run_root apt-get update
        run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y \
            ca-certificates curl jq tar coreutils openssl iproute2 iputils-ping \
            util-linux pciutils lm-sensors openssh-client openssh-server nfs-common nfs-kernel-server
    elif command -v dnf >/dev/null 2>&1; then
        run_root dnf install -y \
            ca-certificates curl jq tar coreutils openssl iproute iputils \
            util-linux pciutils lm_sensors shadow-utils openssh-clients openssh-server nfs-utils
    elif command -v yum >/dev/null 2>&1; then
        run_root yum install -y \
            ca-certificates curl jq tar coreutils openssl iproute iputils \
            util-linux pciutils lm_sensors shadow-utils openssh-clients openssh-server nfs-utils
    elif command -v zypper >/dev/null 2>&1; then
        run_root zypper --non-interactive install \
            ca-certificates curl jq tar coreutils openssl iproute2 iputils \
            util-linux pciutils sensors shadow openssh nfs-client nfs-kernel-server
    elif command -v pacman >/dev/null 2>&1; then
        run_root pacman -S --needed --noconfirm \
            ca-certificates curl jq tar coreutils openssl iproute2 iputils \
            util-linux pciutils lm_sensors shadow openssh nfs-utils
    elif command -v apk >/dev/null 2>&1; then
        run_root apk add \
            ca-certificates curl jq tar coreutils openssl iproute2 iputils \
            util-linux pciutils lm-sensors shadow openssh-client nfs-utils
    elif command -v xbps-install >/dev/null 2>&1; then
        run_root xbps-install -Sy \
            ca-certificates curl jq tar coreutils openssl iproute2 iputils \
            util-linux pciutils lm_sensors shadow openssh nfs-utils
    elif command -v emerge >/dev/null 2>&1; then
        run_root emerge --noreplace \
            net-misc/curl app-misc/jq app-arch/tar sys-apps/coreutils dev-libs/openssl \
            sys-apps/iproute2 net-misc/iputils sys-apps/util-linux \
            sys-apps/pciutils sys-apps/lm-sensors net-misc/openssh net-fs/nfs-utils
    else
        fail "no supported package manager found; install these commands:${missing}"
    fi

    missing=$(missing_runtime_commands)
    [ -z "${missing}" ] || fail "dependency installation did not provide:${missing}"
}

install_dependencies

time_sync_is_ready() {
    if command -v timedatectl >/dev/null 2>&1; then
        synchronized=$(LC_ALL=C timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
        [ "${synchronized}" = "yes" ] && return 0
    fi
    if command -v chronyc >/dev/null 2>&1; then
        LC_ALL=C chronyc tracking 2>/dev/null | grep -Eq '^Leap status[[:space:]]*:[[:space:]]*Normal$' && return 0
    fi
    if command -v ntpq >/dev/null 2>&1; then
        LC_ALL=C ntpq -pn 2>/dev/null | grep -Eq '^\*' && return 0
    fi
    return 1
}

install_time_sync_package() {
    if [ "${SKIP_DEPENDENCIES}" -eq 1 ]; then
        return 1
    fi
    log "Installing Chrony for cluster clock synchronization."
    if command -v apt-get >/dev/null 2>&1; then
        run_root apt-get update
        run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y chrony
    elif command -v dnf >/dev/null 2>&1; then
        run_root dnf install -y chrony
    elif command -v yum >/dev/null 2>&1; then
        run_root yum install -y chrony
    elif command -v zypper >/dev/null 2>&1; then
        run_root zypper --non-interactive install chrony
    elif command -v pacman >/dev/null 2>&1; then
        run_root pacman -S --needed --noconfirm chrony
    elif command -v apk >/dev/null 2>&1; then
        run_root apk add chrony
    elif command -v xbps-install >/dev/null 2>&1; then
        run_root xbps-install -Sy chrony
    elif command -v emerge >/dev/null 2>&1; then
        run_root emerge --noreplace net-misc/chrony
    else
        return 1
    fi
}

enable_time_sync() {
    if command -v systemctl >/dev/null 2>&1; then
        if command -v timedatectl >/dev/null 2>&1; then
            run_root timedatectl set-ntp true >/dev/null 2>&1 || true
        fi
        # Prefer Chrony when present; fall back to the systemd client otherwise.
        for service_name in chronyd chrony systemd-timesyncd; do
            if run_root systemctl cat "${service_name}.service" >/dev/null 2>&1; then
                run_root systemctl enable --now "${service_name}.service" >/dev/null
                break
            fi
        done
    elif command -v rc-update >/dev/null 2>&1; then
        for service_name in chronyd chrony; do
            if [ -x "/etc/init.d/${service_name}" ]; then
                run_root rc-update add "${service_name}" default >/dev/null 2>&1 || true
                run_root rc-service "${service_name}" start >/dev/null 2>&1 || true
                break
            fi
        done
    fi
}

wait_for_time_sync() {
    attempts=0
    while [ "${attempts}" -lt 20 ]; do
        time_sync_is_ready && return 0
        attempts=$((attempts + 1))
        sleep 1
    done
    return 1
}

ensure_time_sync() {
    [ -z "${DESTDIR_VALUE}" ] || return 0
    log "Checking cluster clock synchronization."
    enable_time_sync
    if [ "${ALLOW_UNSYNCHRONIZED_CLOCK}" -eq 1 ]; then
        if time_sync_is_ready; then
            log "System clock is synchronized."
        else
            log "WARNING: continuing without confirmed clock synchronization."
            log "mTLS, OIDC, telemetry ordering, and accounting may fail until time is synchronized."
        fi
        return 0
    fi
    if wait_for_time_sync; then
        log "System clock is synchronized."
        return 0
    fi
    if ! command -v chronyc >/dev/null 2>&1; then
        install_time_sync_package || true
        enable_time_sync
        if wait_for_time_sync; then
            log "System clock is synchronized with Chrony."
            return 0
        fi
    fi
    fail "clock synchronization was not confirmed; check NTP connectivity or rerun with --allow-unsynchronized-clock"
}

ensure_time_sync

if [ -z "${DESTDIR_VALUE}" ] &&
   ! command -v systemctl >/dev/null 2>&1 &&
   ! command -v rc-service >/dev/null 2>&1; then
    fail "XIRANG service installation requires systemd or OpenRC"
fi

TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/xirang-install.XXXXXX")
umask 077

read_token() {
    github_token=${GITHUB_TOKEN:-${GH_TOKEN:-}}
    if [ -z "${github_token}" ]; then
        return
    fi
    case "${github_token}" in
        *[!A-Za-z0-9_.-]*)
            fail "GitHub token contains unsupported characters"
            ;;
    esac
    CURL_CONFIG="${TEMP_DIR}/curl-auth.conf"
    {
        printf 'header = "Authorization: Bearer %s"\n' "${github_token}"
        printf 'header = "X-GitHub-Api-Version: 2022-11-28"\n'
    } > "${CURL_CONFIG}"
    chmod 0600 "${CURL_CONFIG}"
    github_token=
    unset GITHUB_TOKEN GH_TOKEN
}

curl_to_file() {
    output=$1
    accept=$2
    url=$3
    if [ -n "${CURL_CONFIG}" ]; then
        curl --config "${CURL_CONFIG}" --fail --silent --show-error \
            --location --retry 3 --connect-timeout 15 \
            -H "Accept: ${accept}" -o "${output}" "${url}"
    else
        curl --fail --silent --show-error --location --retry 3 \
            --connect-timeout 15 -H "Accept: ${accept}" \
            -o "${output}" "${url}"
    fi
}

archive_name=
checksum_file="${TEMP_DIR}/SHA256SUMS"
signature_file="${TEMP_DIR}/SHA256SUMS.sig"

# verify_release_signature checks the checksum manifest against the release
# signing key embedded above. It reports a missing capability separately from a
# failed check so an old OpenSSL is never mistaken for a tampered release.
verify_release_signature() {
    if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
        fail "openssl is too old to verify Ed25519 release signatures; install OpenSSL 1.1.1 or newer, or pass --sha256"
    fi
    printf '%s\n' "${RELEASE_SIGNING_PUBLIC_KEY}" > "${TEMP_DIR}/release-signing.pub"
    openssl pkeyutl -verify -pubin -inkey "${TEMP_DIR}/release-signing.pub" \
        -rawin -in "${checksum_file}" -sigfile "${signature_file}" >/dev/null 2>&1
}
if [ -n "${ARCHIVE_PATH}" ]; then
    [ -f "${ARCHIVE_PATH}" ] || fail "archive not found: ${ARCHIVE_PATH}"
    ARCHIVE_PATH=$(CDPATH= cd -- "$(dirname -- "${ARCHIVE_PATH}")" && pwd)/$(basename -- "${ARCHIVE_PATH}")
    archive_name=$(basename -- "${ARCHIVE_PATH}")
    case "${archive_name}" in
        *-linux-${ARCH}.tar.gz) ;;
        *-linux-amd64.tar.gz|*-linux-arm64.tar.gz)
            fail "archive architecture does not match this ${ARCH} host"
            ;;
        *) fail "archive name must end in -linux-${ARCH}.tar.gz" ;;
    esac
    adjacent_checksum=$(dirname -- "${ARCHIVE_PATH}")/SHA256SUMS
    if [ -z "${EXPECTED_SHA256}" ] && [ -f "${adjacent_checksum}" ]; then
        cp "${adjacent_checksum}" "${checksum_file}"
        if [ -f "${adjacent_checksum}.sig" ]; then
            cp "${adjacent_checksum}.sig" "${signature_file}"
        fi
    fi
else
    read_token
    metadata="${TEMP_DIR}/release.json"
    if [ "${RELEASE_VERSION}" = "latest" ]; then
        release_endpoint="https://api.github.com/repos/${REPOSITORY}/releases/latest"
    else
        case "${RELEASE_VERSION}" in
            v*) release_tag=${RELEASE_VERSION} ;;
            *) release_tag=v${RELEASE_VERSION} ;;
        esac
        release_endpoint="https://api.github.com/repos/${REPOSITORY}/releases/tags/${release_tag}"
    fi
    log "Reading release information from ${REPOSITORY}."
    if ! curl_to_file "${metadata}" "application/vnd.github+json" "${release_endpoint}"; then
        fail "unable to read the GitHub Release; verify the repository, version, and network connection"
    fi
    asset_url=$(jq -r --arg suffix "-linux-${ARCH}.tar.gz" \
        '[.assets[] | select(.name | endswith($suffix))][0].url // empty' "${metadata}")
    archive_name=$(jq -r --arg suffix "-linux-${ARCH}.tar.gz" \
        '[.assets[] | select(.name | endswith($suffix))][0].name // empty' "${metadata}")
    checksum_url=$(jq -r \
        '[.assets[] | select(.name == "SHA256SUMS")][0].url // empty' "${metadata}")
    signature_url=$(jq -r \
        '[.assets[] | select(.name == "SHA256SUMS.sig")][0].url // empty' "${metadata}")
    [ -n "${asset_url}" ] ||
        fail "release has no Linux ${ARCH} archive"
    [ -n "${checksum_url}" ] ||
        fail "release has no SHA256SUMS asset"

    ARCHIVE_PATH="${TEMP_DIR}/${archive_name}"
    log "Downloading ${archive_name}."
    curl_to_file "${ARCHIVE_PATH}" "application/octet-stream" "${asset_url}"
    curl_to_file "${checksum_file}" "application/octet-stream" "${checksum_url}"
    if [ -n "${signature_url}" ]; then
        curl_to_file "${signature_file}" "application/octet-stream" "${signature_url}"
    fi
fi

# The signature is what distinguishes a genuine release from a substituted one.
# It is required whenever trust rests on the downloaded manifest; passing
# --sha256 establishes the same trust out of band and takes its place.
if [ -z "${EXPECTED_SHA256}" ]; then
    [ -s "${signature_file}" ] ||
        fail "release is not signed; publish SHA256SUMS.sig alongside it, or pass --sha256 with a digest you trust"
    verify_release_signature ||
        fail "SHA256SUMS signature does not match the XIRANG release signing key; do not install this package"
    log "Release signature verified."
fi

case "${EXPECTED_SHA256}" in
    "")
        [ -f "${checksum_file}" ] ||
            fail "no trusted checksum found; provide --sha256 or an adjacent SHA256SUMS"
        EXPECTED_SHA256=$(awk -v file="${archive_name}" '
            $2 == file || $2 == "*" file ||
            $2 == "dist/" file || $2 == "*dist/" file {
                print $1
                exit
            }
        ' "${checksum_file}")
        [ -n "${EXPECTED_SHA256}" ] ||
            fail "SHA256SUMS has no entry for ${archive_name}"
        ;;
esac

case "${EXPECTED_SHA256}" in
    *[!0-9A-Fa-f]*|"")
        fail "expected SHA-256 must contain exactly 64 hexadecimal characters"
        ;;
esac
[ "${#EXPECTED_SHA256}" -eq 64 ] ||
    fail "expected SHA-256 must contain exactly 64 hexadecimal characters"

actual_sha256=$(sha256sum "${ARCHIVE_PATH}" | awk '{print $1}')
expected_lower=$(printf '%s' "${EXPECTED_SHA256}" | tr 'A-F' 'a-f')
[ "${actual_sha256}" = "${expected_lower}" ] ||
    fail "checksum mismatch for ${archive_name}"
log "SHA-256 verified."

if ! tar -tzf "${ARCHIVE_PATH}" | awk '
    /^\// || /(^|\/)\.\.(\/|$)/ { unsafe = 1 }
    END { exit unsafe }
'; then
    fail "archive contains an unsafe path"
fi
if ! tar -tvzf "${ARCHIVE_PATH}" | awk '
    substr($0, 1, 1) != "d" && substr($0, 1, 1) != "-" { unsafe = 1 }
    END { exit unsafe }
'; then
    fail "archive contains links or special files"
fi

extract_dir="${TEMP_DIR}/payload"
mkdir -p "${extract_dir}"
tar -xzf "${ARCHIVE_PATH}" -C "${extract_dir}"
package_root=$(find "${extract_dir}" -mindepth 1 -maxdepth 1 -type d \
    -name "xirang-*-linux-${ARCH}" -print | sed -n '1p')
[ -n "${package_root}" ] || fail "release archive has an unexpected layout"
package_root_count=$(find "${extract_dir}" -mindepth 1 -maxdepth 1 -type d \
    -name "xirang-*-linux-${ARCH}" -print | wc -l)
[ "${package_root_count}" -eq 1 ] ||
    fail "release archive must contain exactly one package directory"
[ ! -L "${package_root}/deploy/install/install.sh" ] ||
    fail "release installer must not be a symbolic link"
[ -x "${package_root}/deploy/install/install.sh" ] ||
    fail "release archive is missing deploy/install/install.sh"

log "Installing ${archive_name}."
if [ -n "${DESTDIR_VALUE}" ]; then
    DESTDIR="${DESTDIR_VALUE}" SOURCE_DIR="${package_root}" \
        XIRANG_NONINTERACTIVE=1 sh "${package_root}/deploy/install/install.sh"
else
    run_root env \
        SOURCE_DIR="${package_root}" \
        XIRANG_NONINTERACTIVE="${NONINTERACTIVE}" \
        sh "${package_root}/deploy/install/install.sh"
fi

if [ "${NONINTERACTIVE}" -eq 1 ]; then
    log "Package installation completed. Run the service setup manually."
else
    log "XIRANG installation and guided initialization completed."
fi
