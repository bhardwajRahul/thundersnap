# $1 == 1 for initial installation.
# $1 == 2 for upgrades.

if [ $1 -eq 1 ] ; then
    systemctl preset thundersnapd.service >/dev/null 2>&1 || :
fi

if [ $1 -eq 2 ] ; then
    systemctl try-restart thundersnapd.service >/dev/null 2>&1 || :
fi
