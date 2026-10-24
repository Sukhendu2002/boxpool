#!/bin/sh
# Ad-hoc signs a development build with the virtualization entitlement, which
# macOS requires of any process that runs VMs. Releases are signed with a
# Developer ID instead (#24).
set -eu
if [ "$#" -ne 1 ]; then
    echo "usage: $0 <binary>" >&2
    exit 2
fi
codesign --force --sign - --entitlements "$(dirname "$0")/vz.entitlements" "$1"
