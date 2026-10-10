#!/bin/sh

set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/docker-compose.auto-tls.example.yml"
env_file=''
env_file_count=0
check_only=false
expect_env_file=false

for argument do
    if [ "$expect_env_file" = true ]; then
        env_file=$argument
        env_file_count=$((env_file_count + 1))
        expect_env_file=false
        continue
    fi

    case $argument in
        --env-file)
            expect_env_file=true
            ;;
        --env-file=*)
            env_file=${argument#--env-file=}
            env_file_count=$((env_file_count + 1))
            ;;
        --check)
            check_only=true
            ;;
    esac
done

if [ "$expect_env_file" = true ]; then
    printf '%s\n' 'Automatic TLS preflight failed: --env-file requires a path.' >&2
    exit 1
fi

if [ "$env_file_count" -gt 1 ]; then
    printf '%s\n' 'Automatic TLS preflight failed: only one --env-file is supported.' >&2
    exit 1
fi

if [ "$env_file_count" -eq 1 ] && [ -z "$env_file" ]; then
    printf '%s\n' 'Automatic TLS preflight failed: --env-file requires a path.' >&2
    exit 1
fi

# Compose, rather than this shell, must expand the interpolation expressions.
# shellcheck disable=SC2016
compose_probe='services:
  preflight:
    image: scratch
    environment:
      WINGS_API_PORT: "${WINGS_API_PORT-}"
      WINGS_SFTP_PORT: "${WINGS_SFTP_PORT-}"'

if [ "$env_file_count" -eq 1 ]; then
    if ! compose_environment=$(printf '%s\n' "$compose_probe" | docker compose \
        --project-directory "$script_dir" \
        --env-file "$env_file" \
        -f - \
        config --environment); then
        printf '%s\n' 'Automatic TLS preflight failed: Docker Compose could not resolve the environment.' >&2
        exit 1
    fi
else
    if ! compose_environment=$(printf '%s\n' "$compose_probe" | docker compose \
        --project-directory "$script_dir" \
        -f - \
        config --environment); then
        printf '%s\n' 'Automatic TLS preflight failed: Docker Compose could not resolve the environment.' >&2
        exit 1
    fi
fi

read_resolved_value() {
    variable_name=$1

    printf '%s\n' "$compose_environment" | awk -v wanted="$variable_name" '
        index($0, wanted "=") == 1 {
            print substr($0, length(wanted) + 2)
            exit
        }
    '
}

api_port=$(read_resolved_value WINGS_API_PORT)
sftp_port=$(read_resolved_value WINGS_SFTP_PORT)

validate_port() {
    variable_name=$1
    port=$2

    if [ -z "$port" ]; then
        printf 'Automatic TLS preflight failed: %s is required.\n' "$variable_name" >&2
        exit 1
    fi

    case $port in
        *[!0-9]*)
            printf 'Automatic TLS preflight failed: %s must be a numeric port from 1 through 65535.\n' "$variable_name" >&2
            exit 1
            ;;
    esac

    if ! awk -v port="$port" 'BEGIN { exit !(port >= 1 && port <= 65535) }'; then
        printf 'Automatic TLS preflight failed: %s must be a numeric port from 1 through 65535.\n' "$variable_name" >&2
        exit 1
    fi

    if awk -v port="$port" 'BEGIN { exit !(port == 80) }'; then
        printf 'Automatic TLS preflight failed: %s cannot use port 80; Wings reserves it for the ACME HTTP-01 challenge.\n' "$variable_name" >&2
        exit 1
    fi
}

validate_port WINGS_API_PORT "$api_port"
validate_port WINGS_SFTP_PORT "$sftp_port"

if awk -v api="$api_port" -v sftp="$sftp_port" 'BEGIN { exit !(api == sftp) }'; then
    printf '%s\n' 'Automatic TLS preflight failed: WINGS_API_PORT and WINGS_SFTP_PORT must be different.' >&2
    exit 1
fi

printf 'Automatic TLS port preflight passed (API %s, SFTP %s).\n' "$api_port" "$sftp_port"

if [ "$check_only" = true ]; then
    exit 0
fi

exec docker compose --project-directory "$script_dir" -f "$compose_file" "$@"
