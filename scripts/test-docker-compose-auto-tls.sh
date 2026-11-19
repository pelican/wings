#!/bin/sh

set -eu

repository_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
wrapper="$repository_root/docker-compose.auto-tls.sh"
empty_env=$(mktemp)
configured_env=$(mktemp)
project_directory=$(mktemp -d)
working_directory=$(mktemp -d)
trap 'rm -f "$empty_env" "$configured_env"; rm -rf "$project_directory" "$working_directory"' EXIT HUP INT TERM

printf '%s\n' \
    'WINGS_API_PORT="8443"' \
    'WINGS_SFTP_PORT=2022 # default SFTP port' \
    > "$configured_env"

cp "$wrapper" "$project_directory/docker-compose.auto-tls.sh"
cp "$repository_root/docker-compose.auto-tls.example.yml" \
    "$project_directory/docker-compose.auto-tls.example.yml"
# Preserve these expressions literally so Docker Compose resolves them.
# shellcheck disable=SC2016
printf '%s\n' \
    'DEFAULT_API_PORT=8443' \
    'WINGS_API_PORT=${DEFAULT_API_PORT:-443}' \
    'WINGS_SFTP_PORT=${DEFAULT_SFTP_PORT:-2022}' \
    > "$project_directory/.env"

expect_success() {
    description=$1
    shift

    if ! output=$("$@" 2>&1); then
        printf 'FAIL: %s\n%s\n' "$description" "$output" >&2
        exit 1
    fi
}

expect_failure() {
    description=$1
    expected=$2
    shift 2

    if output=$("$@" 2>&1); then
        printf 'FAIL: %s unexpectedly passed.\n' "$description" >&2
        exit 1
    fi

    if ! printf '%s\n' "$output" | grep -F "$expected" >/dev/null; then
        printf 'FAIL: %s returned an unexpected error.\n%s\n' "$description" "$output" >&2
        exit 1
    fi
}

expect_failure 'unset API port' 'WINGS_API_PORT is required' \
    env -u WINGS_API_PORT -u WINGS_SFTP_PORT sh "$wrapper" --env-file "$empty_env" --check
expect_failure 'non-numeric API port' 'WINGS_API_PORT must be a numeric port from 1 through 65535' \
    env WINGS_API_PORT=invalid WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_failure 'port below the lower boundary' 'WINGS_API_PORT must be a numeric port from 1 through 65535' \
    env WINGS_API_PORT=0 WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_failure 'port above the upper boundary' 'WINGS_API_PORT must be a numeric port from 1 through 65535' \
    env WINGS_API_PORT=65536 WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_failure 'reserved ACME port' 'WINGS_API_PORT cannot use port 80' \
    env WINGS_API_PORT=80 WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_failure 'duplicate service ports' 'WINGS_API_PORT and WINGS_SFTP_PORT must be different' \
    env WINGS_API_PORT=2022 WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_failure 'multiple environment files' 'only one --env-file is supported' \
    env WINGS_API_PORT=8443 WINGS_SFTP_PORT=2022 sh "$wrapper" \
        --env-file "$empty_env" --env-file "$configured_env" --check

expect_success 'lower and upper boundaries' \
    env WINGS_API_PORT=1 WINGS_SFTP_PORT=65535 sh "$wrapper" --check
expect_success 'typical API and SFTP ports' \
    env WINGS_API_PORT=8443 WINGS_SFTP_PORT=2022 sh "$wrapper" --check
expect_success 'ports loaded from a Compose environment file' \
    env -u WINGS_API_PORT -u WINGS_SFTP_PORT sh "$wrapper" --env-file "$configured_env" --check
# Positional parameters are intentionally expanded by the child shell.
# shellcheck disable=SC2016
expect_success 'project environment interpolation from another working directory' \
    env -u WINGS_API_PORT -u WINGS_SFTP_PORT sh -c \
        'cd "$1" && sh "$2/docker-compose.auto-tls.sh" --check' \
        test "$working_directory" "$project_directory"

printf '%s\n' 'Automatic TLS Compose preflight tests passed.'
