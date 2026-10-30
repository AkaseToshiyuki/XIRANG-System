#!/bin/sh
set -eu

PREFIX=${PREFIX:-/usr/local}
DESTDIR=${DESTDIR:-}
SOURCE_DIR=${SOURCE_DIR:-$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)}

create_service_user() {
    account=$1
    home=$2
    if id "${account}" >/dev/null 2>&1; then
        return
    fi
    task_shell=/bin/false
    if command -v nologin >/dev/null 2>&1; then
        task_shell=$(command -v nologin)
    fi
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --home-dir "${home}" --shell "${task_shell}" "${account}"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -h "${home}" -s "${task_shell}" "${account}"
    else
        echo "Unable to create ${account}: install useradd or adduser" >&2
        exit 1
    fi
}

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ]; then
    create_service_user xirang /var/lib/xirang
    create_service_user xirang-task /nonexistent
    create_service_user xirang-build /nonexistent
    create_service_user xirang-operator /var/lib/xirang-operator
fi

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ] && getent group xirang-task >/dev/null 2>&1; then
    for account in xirang xirang-build; do
        if id "${account}" >/dev/null 2>&1; then
            if command -v usermod >/dev/null 2>&1; then
                usermod -a -G xirang-task "${account}"
            elif command -v addgroup >/dev/null 2>&1; then
                addgroup "${account}" xirang-task >/dev/null 2>&1 || true
            fi
        fi
    done
fi

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ] && id xirang-task >/dev/null 2>&1; then
    for device_group in render video rdma; do
        if getent group "${device_group}" >/dev/null 2>&1; then
            if command -v usermod >/dev/null 2>&1; then
                usermod -a -G "${device_group}" xirang-task
            elif command -v addgroup >/dev/null 2>&1; then
                addgroup xirang-task "${device_group}" >/dev/null 2>&1 || true
            fi
        fi
    done
fi

install -d "${DESTDIR}${PREFIX}/bin"
install -m 0755 "${SOURCE_DIR}/bin/xirangd" "${DESTDIR}${PREFIX}/bin/xirangd"
install -m 0755 "${SOURCE_DIR}/bin/xirang-agent" "${DESTDIR}${PREFIX}/bin/xirang-agent"
install -m 0755 "${SOURCE_DIR}/bin/xirangctl" "${DESTDIR}${PREFIX}/bin/xirangctl"
install -m 0755 "${SOURCE_DIR}/bin/xirang-maint" "${DESTDIR}${PREFIX}/bin/xirang-maint"
if [ -f "${SOURCE_DIR}/bin/xirang-mpi-adapter" ]; then
    install -m 0755 "${SOURCE_DIR}/bin/xirang-mpi-adapter" "${DESTDIR}${PREFIX}/bin/xirang-mpi-adapter"
    ln -sf xirang-mpi-adapter "${DESTDIR}${PREFIX}/bin/xirang-mpi-adapter-openmpi"
    ln -sf xirang-mpi-adapter "${DESTDIR}${PREFIX}/bin/xirang-mpi-adapter-mpich"
fi

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ]; then
    install -d -m 0750 -o xirang -g xirang /var/lib/xirang
    if command -v mountpoint >/dev/null 2>&1 && mountpoint -q /srv/xirang/XirangShare; then
        # NFS all_squash intentionally rejects client-side ownership changes.
        # The storage node owns this already-mounted tree and its export policy.
        test -d /srv/xirang/XirangShare/.xirang/builds
    else
        # Preparing/exporting a share must preserve host ownership and access.
        # The helper grants the cluster group access through a POSIX ACL.
        "${PREFIX}/bin/xirang-agent" mount-helper prepare share_default /srv/xirang/XirangShare
        install -d -m 2770 -o xirang-task -g xirang-task \
            /srv/xirang/XirangShare/.xirang \
            /srv/xirang/XirangShare/.xirang/builds \
            /srv/xirang/XirangShare/.xirang/builds/sources \
            /srv/xirang/XirangShare/.xirang/builds/work \
            /srv/xirang/XirangShare/.xirang/builds/artifacts
    fi
    install -d -m 0700 -o root -g root /var/lib/xirang-agent
    install -d -m 0711 -o root -g root /var/lib/xirang-tasks
    install -d -m 0711 -o root -g root /var/lib/xirang-build
    install -d -m 0770 -o root -g xirang /var/lib/xirang-build/locks
    build_lock=/var/lib/xirang-build/locks/$(id -u xirang-build).lock
    if [ ! -e "${build_lock}" ]; then
        install -m 0660 -o root -g xirang /dev/null "${build_lock}"
    fi
    operator_group=$(id -gn xirang-operator)
    install -d -m 0700 -o xirang-operator -g "${operator_group}" /var/lib/xirang-operator
    install -d -m 0750 -o root -g xirang /etc/xirang
fi

if [ -d "${SOURCE_DIR}/web/dist" ]; then
    install -d "${DESTDIR}${PREFIX}/share/xirang/web"
    cp -R "${SOURCE_DIR}/web/dist/." "${DESTDIR}${PREFIX}/share/xirang/web/"
    find "${DESTDIR}${PREFIX}/share/xirang/web" -type d -exec chmod 0755 {} +
    find "${DESTDIR}${PREFIX}/share/xirang/web" -type f -exec chmod 0644 {} +
elif [ -d "${SOURCE_DIR}/share/xirang/web" ]; then
    install -d "${DESTDIR}${PREFIX}/share/xirang/web"
    cp -R "${SOURCE_DIR}/share/xirang/web/." "${DESTDIR}${PREFIX}/share/xirang/web/"
    find "${DESTDIR}${PREFIX}/share/xirang/web" -type d -exec chmod 0755 {} +
    find "${DESTDIR}${PREFIX}/share/xirang/web" -type f -exec chmod 0644 {} +
fi

install_job_artifacts=1
if [ -z "${DESTDIR}" ] && command -v mountpoint >/dev/null 2>&1 && mountpoint -q /srv/xirang/XirangShare; then
    # Managed nodes consume the controller/storage-node copy through NFS. They
    # must not rewrite the export, especially when all_squash is enabled.
    install_job_artifacts=0
fi
if [ "${XIRANG_INSTALL_JOB_ARTIFACTS:-${install_job_artifacts}}" = "1" ] && [ -d "${SOURCE_DIR}/deploy/job-artifacts" ]; then
    job_artifact_root=${XIRANG_JOB_ARTIFACT_ROOT:-/srv/xirang/XirangShare/.xirang/artifacts}
    install -d "${DESTDIR}${job_artifact_root}"
    cp -R "${SOURCE_DIR}/deploy/job-artifacts/." "${DESTDIR}${job_artifact_root}/"
    find "${DESTDIR}${job_artifact_root}" -type d -exec chmod 0755 {} +
    find "${DESTDIR}${job_artifact_root}" -type f -exec chmod 0755 {} +
fi

if [ -d "${SOURCE_DIR}/deploy/systemd" ] && { [ -d /run/systemd/system ] || command -v systemctl >/dev/null 2>&1; }; then
    install -d "${DESTDIR}/etc/systemd/system"
    install -m 0644 "${SOURCE_DIR}"/deploy/systemd/*.service "${DESTDIR}/etc/systemd/system/"
elif [ -d "${SOURCE_DIR}/deploy/openrc" ]; then
    install -d "${DESTDIR}/etc/init.d"
    install -m 0755 "${SOURCE_DIR}"/deploy/openrc/* "${DESTDIR}/etc/init.d/"
fi
if [ -d "${SOURCE_DIR}/deploy/config" ]; then
    install -d "${DESTDIR}/etc/xirang"
    install -m 0640 "${SOURCE_DIR}"/deploy/config/*.example "${DESTDIR}/etc/xirang/"
fi

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ] && command -v systemctl >/dev/null 2>&1; then
    "${PREFIX}/bin/xirang-agent" mount-helper policy-authorize default-share /srv/xirang/XirangShare
    if [ -f /etc/fstab ]; then
        awk '/# xirang:[A-Za-z0-9_-]+$/ { id=$NF; sub(/^xirang:/, "", id); print id, $2 }' /etc/fstab |
        while read -r policy_id policy_path; do
            "${PREFIX}/bin/xirang-agent" mount-helper policy-authorize "${policy_id}-mount" "${policy_path}"
        done
    fi
fi

echo "Installed XIRANG under ${DESTDIR}${PREFIX}"
echo "Review /etc/xirang environment files and enable the matching systemd or OpenRC services."

if [ -z "${DESTDIR}" ] && [ "$(id -u)" -eq 0 ] && [ -t 0 ] && [ "${XIRANG_NONINTERACTIVE:-0}" != "1" ]; then
    printf "Deploy this machine as a controller or managed node? [controller/node]: "
    read -r deployment_role
    case "${deployment_role}" in
        controller)
            printf "Controller IP address used by managed nodes: "
            read -r controller_ip
            if [ -z "${controller_ip}" ]; then
                echo "Controller IP address is required." >&2
                exit 2
            fi
            controller_host=${controller_ip}
            case "${controller_ip}" in
                *:*) controller_host="[${controller_ip}]" ;;
            esac
            umask 027
            {
                printf "XIRANGD_ARGS=\"-tls-host=%s -agent-listen=%s:8444 -discovery-controller-url=https://%s:8899\"\n" \
                    "${controller_ip}" "${controller_host}" "${controller_host}"
            } > /etc/xirang/xirangd.env
            chmod 0640 /etc/xirang/xirangd.env
            if command -v systemctl >/dev/null 2>&1; then
                systemctl daemon-reload
                systemctl enable --now xirangd
            elif command -v rc-update >/dev/null 2>&1; then
                rc-update add xirangd default
                rc-service xirangd start
            fi
            controller_url="https://${controller_host}:8899"
            attempts=0
            until "${PREFIX}/bin/xirangctl" \
                -server="${controller_url}" \
                -ca-file=/var/lib/xirang/pki/ca.crt \
                -token-file=/var/lib/xirang/admin.token \
                status >/dev/null 2>&1; do
                attempts=$((attempts + 1))
                if [ "${attempts}" -ge 30 ]; then
                    echo "Controller did not become ready in time." >&2
                    exit 1
                fi
                sleep 1
            done
            token_json=$("${PREFIX}/bin/xirangctl" \
                -server="${controller_url}" \
                -ca-file=/var/lib/xirang/pki/ca.crt \
                -token-file=/var/lib/xirang/admin.token \
                token create -name "local controller Agent" -ttl=15m -max-uses=1 -require-approval=false)
            enrollment_token=$(printf "%s\n" "${token_json}" | sed -n 's/^[[:space:]]*"secret":[[:space:]]*"\([^"]*\)".*/\1/p')
            if [ -z "${enrollment_token}" ]; then
                echo "Unable to create local controller Agent enrollment token." >&2
                exit 1
            fi
            token_path=/etc/xirang/enrollment.token
            umask 077
            printf "%s\n" "${enrollment_token}" > "${token_path}"
            enrollment_result=$("${PREFIX}/bin/xirang-agent" enroll \
                -server="${controller_url}" \
                -ca-file=/var/lib/xirang/pki/ca.crt \
                -state-dir=/var/lib/xirang-agent \
                -token-file="${token_path}")
            rm -f "${token_path}"
            controller_node_id=$(printf "%s\n" "${enrollment_result}" | sed -n 's/^enrolled node \([^;]*\);.*/\1/p')
            if [ -z "${controller_node_id}" ]; then
                echo "Unable to identify the local controller Agent after enrollment." >&2
                exit 1
            fi
            controller_ssh_user=${SUDO_USER:-root}
            if ! id "${controller_ssh_user}" >/dev/null 2>&1; then
                echo "Unable to configure controller SSH identity ${controller_ssh_user}." >&2
                exit 1
            fi
            controller_home=$(getent passwd "${controller_ssh_user}" | awk -F: '{print $6}')
            controller_group=$(id -gn "${controller_ssh_user}")
            if [ -z "${controller_home}" ] || [ ! -d "${controller_home}" ]; then
                echo "Controller SSH account requires an existing home directory." >&2
                exit 1
            fi
            install -d -m 0700 -o "${controller_ssh_user}" -g "${controller_group}" "${controller_home}/.ssh"
            touch "${controller_home}/.ssh/authorized_keys"
            controller_public_key=$(cat /var/lib/xirang/ssh/id_ed25519.pub)
            if ! grep -qxF "${controller_public_key}" "${controller_home}/.ssh/authorized_keys"; then
                printf "%s\n" "${controller_public_key}" >> "${controller_home}/.ssh/authorized_keys"
            fi
            chown "${controller_ssh_user}:${controller_group}" "${controller_home}/.ssh/authorized_keys"
            chmod 0600 "${controller_home}/.ssh/authorized_keys"
            sudoers_path=/etc/sudoers.d/xirang-mount-helper
            # Mount/export mutations now use the opt-in Agent mTLS capability.
            # SSH remains a trusted-administrator path for file data and stats.
            : > "${sudoers_path}"
            for file_action in stats file-list file-read file-write file-mkdir file-rename file-delete; do
                printf "%s ALL=(root) NOPASSWD: %s/bin/xirang-agent mount-helper %s *\n" \
                    "${controller_ssh_user}" "${PREFIX}" "${file_action}" >> "${sudoers_path}"
            done
            chmod 0440 "${sudoers_path}"
            if command -v visudo >/dev/null 2>&1; then
                visudo -cf "${sudoers_path}" >/dev/null
            fi
            "${PREFIX}/bin/xirangctl" \
                -server="${controller_url}" \
                -ca-file=/var/lib/xirang/pki/ca.crt \
                -token-file=/var/lib/xirang/admin.token \
                node-configure "${controller_node_id}" \
                -roles=management,storage \
                -management-address="${controller_ip}" \
                -ssh-user="${controller_ssh_user}"
            if command -v systemctl >/dev/null 2>&1; then
                systemctl enable --now xirang-agent
            elif command -v rc-update >/dev/null 2>&1; then
                rc-update add xirang-agent default
                rc-service xirang-agent start
            fi
            echo "Controller deployment completed."
            echo "Open https://${controller_host}:8899 to create the first administrator."
            ;;
        node|worker)
            wait_for_approval=0
            printf "Join now with controller IP and token, or wait for Web approval? [direct/wait]: "
            read -r enrollment_mode
            case "${enrollment_mode}" in
                direct)
                    printf "Controller IP address: "
                    read -r controller_ip
                    if [ -z "${controller_ip}" ]; then
                        echo "Controller IP address is required." >&2
                        exit 2
                    fi
                    controller_host=${controller_ip}
                    case "${controller_ip}" in
                        *:*) controller_host="[${controller_ip}]" ;;
                    esac
                    controller_url="https://${controller_host}:8899"
                    token_path=/etc/xirang/enrollment.token
                    trap 'stty echo 2>/dev/null || true; rm -f "${token_path}"' EXIT HUP INT TERM
                    printf "Cluster join token: "
                    stty -echo
                    read -r enrollment_token
                    stty echo
                    printf "\n"
                    umask 077
                    printf "%s\n" "${enrollment_token}" > "${token_path}"
                    "${PREFIX}/bin/xirang-agent" enroll \
                        -server="${controller_url}" \
                        -ca-file=auto \
                        -state-dir=/var/lib/xirang-agent \
                        -token-file="${token_path}"
                    rm -f "${token_path}"
                    trap - EXIT HUP INT TERM
                    ;;
                wait)
                    wait_for_approval=1
                    echo "The Agent will wait on TLS bootstrap port 8844."
                    echo "In the controller Web console, enter this node's IP and approve it with the one-time code printed below."
                    ;;
                *)
                    echo "Unknown managed-node enrollment mode." >&2
                    exit 2
                    ;;
            esac
            if command -v systemctl >/dev/null 2>&1; then
                systemctl daemon-reload
                systemctl enable --now xirang-agent
            elif command -v rc-update >/dev/null 2>&1; then
                rc-update add xirang-agent default
                rc-service xirang-agent start
            fi
            if [ "${wait_for_approval}" -eq 1 ]; then
                pairing_code_file=/var/lib/xirang-agent/.bootstrap-pairing-code
                pairing_wait=0
                while [ ! -s "${pairing_code_file}" ] && [ "${pairing_wait}" -lt 10 ]; do
                    sleep 1
                    pairing_wait=$((pairing_wait + 1))
                done
                if [ -s "${pairing_code_file}" ]; then
                    printf "One-time Agent pairing code: %s\n" "$(tr -d '\r\n' < "${pairing_code_file}")"
                else
                    echo "The pairing code is not ready; inspect the xirang-agent service log." >&2
                fi
            fi
            echo "Managed-node deployment completed."
            ;;
        *)
            echo "Unknown deployment role; installation is complete but no service was enrolled." >&2
            ;;
    esac
fi
