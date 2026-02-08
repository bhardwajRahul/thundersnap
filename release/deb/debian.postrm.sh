#!/bin/sh
set -e
if [ -d /run/systemd/system ] ; then
	systemctl --system daemon-reload >/dev/null || true
fi

if [ -x "/usr/bin/deb-systemd-helper" ]; then
    if [ "$1" = "remove" ]; then
		deb-systemd-helper mask 'thundersnapd.service' >/dev/null || true
	fi

    if [ "$1" = "purge" ]; then
		deb-systemd-helper purge 'thundersnapd.service' >/dev/null || true
		deb-systemd-helper unmask 'thundersnapd.service' >/dev/null || true
		rm -rf /var/lib/thundersnap
	fi
fi
