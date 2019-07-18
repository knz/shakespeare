#!/bin/sh
shorthash=$(git log -n 1  --pretty=format:%h)
dt=$(git log -n 1 --pretty=format:%cd --date=format:%Y%m%d)
descfull=$(git describe --always --tags)
descdirty=$(git describe --always --tags --dirty)
ch=
if [ "$descfull" != "$descdirty" ]; then
	ch=", with changes"
fi
words=$(go run pkg/namegen/main.go pkg/namegen/words.go $shorthash "$ch")
cat >pkg/cmd/version.go <<EOF
// Generated code (namegen)
package cmd

const versionName = "$words ($dt-$shorthash$ch)"
EOF
