#!/bin/sh
set -eu
(
	echo "package cmd"
	echo
	echo "// Note: the following is automatically generated from report.html."
	printf "const reportHTML = \`"
	sed -e 's/^[ 	]*//g' < pkg/cmd/report.html | grep -v '^$'
	printf "\`"
) >pkg/cmd/report_html.go
