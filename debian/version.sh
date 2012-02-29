#!/bin/sh
set -eu
echo $(hg tags | sed -e "s/:.*$//" | awk "BEGIN { max = 0 } $(hg identify -n) == \$2 && \$1~/^${1}\./ {if (\$2 > max) { max = \$2; tag = \$1; } } END { print tag, max; }") > VERSION
