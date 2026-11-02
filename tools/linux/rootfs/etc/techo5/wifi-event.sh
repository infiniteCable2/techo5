#!/bin/sh
# Association is a link fact, not proof of an address. The address owner decides
# whether this network needs a fresh lease; duplicate CONNECTED events are safe.
case "$1:$2" in
wlan0:CONNECTED)
	exec /usr/local/sbin/techo5-net up
	;;
esac
