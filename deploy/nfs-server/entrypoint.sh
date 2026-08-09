#!/bin/sh
set -eu

SHARED_DIRECTORY="${SHARED_DIRECTORY:?SHARED_DIRECTORY must be set}"
PERMITTED="${PERMITTED:-*}"
READ_ONLY="${READ_ONLY:-false}"
SYNC="${SYNC:-false}"

mkdir -p "$SHARED_DIRECTORY"
# sandboxd's NFS client authenticates as uid:gid 1000:1000 (matching the
# sandbox container's fixed user, see internal/dockerengine/client.go), so
# the export root must be writable by that uid or every MakePath/create
# fails with NFS4ERR_ACCESS regardless of no_root_squash.
chown 1000:1000 "$SHARED_DIRECTORY"

if [ "$READ_ONLY" = "true" ]; then
  access_mode=ro
else
  access_mode=rw
fi

if [ "$SYNC" = "true" ]; then
  sync_mode=sync
else
  sync_mode=async
fi

# fsid=0 marks this export as the NFSv4 pseudo-root, so clients can mount it
# as ":${SHARED_DIRECTORY}" without needing the export path in the mount device.
echo "${SHARED_DIRECTORY} ${PERMITTED}(${access_mode},fsid=0,${sync_mode},no_subtree_check,no_auth_nlm,insecure,no_root_squash)" > /etc/exports
cat /etc/exports

mountpoint -q /proc/fs/nfsd || mount -t nfsd nfsd /proc/fs/nfsd

rpcbind -w
rpc.statd --no-notify
exportfs -ra
rpc.nfsd -N 3 4
rpc.mountd -V 4

shutdown() {
  echo "Shutting down NFS server"
  exportfs -ua
  rpc.nfsd 0
  exit 0
}
trap shutdown TERM INT

echo "NFS server ready, exporting ${SHARED_DIRECTORY}"
tail -f /dev/null &
wait $!
