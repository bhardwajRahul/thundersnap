# $1 == 0 for uninstallation.
# $1 == 1 for removing old package during upgrade.

if [ $1 -eq 0 ] ; then
        systemctl --no-reload disable thundersnapd.service > /dev/null 2>&1 || :
        systemctl stop thundersnapd.service > /dev/null 2>&1 || :
fi
