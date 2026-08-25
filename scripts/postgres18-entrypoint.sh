#!/bin/sh
set -eu

postgres_root=${DP1_POSTGRES_ROOT:-/var/lib/postgresql}
active_pgdata=${PGDATA:-"$postgres_root/${PG_MAJOR:-18}/docker"}

# PostgreSQL 18 moved the image volume to /var/lib/postgresql. Reusing a named
# volume from the old /var/lib/postgresql/data mount exposes the legacy cluster
# outside PGDATA, where the stock entrypoint would otherwise initialize a new,
# apparently empty cluster.
if [ ! -f "$active_pgdata/PG_VERSION" ] && {
	[ -f "$postgres_root/PG_VERSION" ] || [ -f "$postgres_root/data/PG_VERSION" ]
}; then
	echo >&2 "Legacy PostgreSQL data detected outside PGDATA ($active_pgdata)."
	echo >&2 "Refusing to initialize a new cluster over the existing Compose volume."
	echo >&2 "Follow the PostgreSQL volume migration steps in DEVELOPMENT.md, then retry."
	exit 1
fi

exec docker-entrypoint.sh "$@"
