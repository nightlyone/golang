#!/usr/bin/env bash
# Copyright 2009 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -e

eval $(go tool dist env)

unset CDPATH	# in case user has it set

# no core files, please
ulimit -c 0

# allow all.bash to avoid double-build of everything
rebuild=true
if [ "$1" = "--no-rebuild" ]; then
	shift
else
	echo '# Building packages and commands.'
	time go install -a -v std
	echo
fi

echo '# Testing packages.'
time go test std -short -timeout=120s
echo

echo '# runtime -cpu=1,2,4'
go test runtime -short -timeout=120s -cpu=1,2,4
echo

echo '# sync -cpu=10'
go test sync -short -timeout=120s -cpu=10

xcd() {
	echo
	echo --- cd $1
	builtin cd "$GOROOT"/src/$1
}

BROKEN=true

$BROKEN ||
[ "$CGO_ENABLED" != 1 ] ||
[ "$GOHOSTOS" == windows ] ||
(xcd ../misc/cgo/stdio
"$GOMAKE" clean
./test.bash
) || exit $?

$BROKEN ||
[ "$CGO_ENABLED" != 1 ] ||
(xcd ../misc/cgo/life
"$GOMAKE" clean
./test.bash
) || exit $?

$BROKEN ||
[ "$CGO_ENABLED" != 1 ] ||
(xcd ../misc/cgo/test
"$GOMAKE" clean
gotest
) || exit $?

$BROKEN ||
[ "$CGO_ENABLED" != 1 ] ||
[ "$GOHOSTOS" == windows ] ||
[ "$GOHOSTOS" == darwin ] ||
(xcd ../misc/cgo/testso
"$GOMAKE" clean
./test.bash
) || exit $?

$BROKEN ||
(xcd ../doc/progs
time ./run
) || exit $?

$BROKEN ||
[ "$GOARCH" == arm ] ||  # uses network, fails under QEMU
(xcd ../doc/codelab/wiki
"$GOMAKE" clean
"$GOMAKE"
"$GOMAKE" test
) || exit $?

$BROKEN ||
for i in ../misc/dashboard/builder ../misc/goplay
do
	(xcd $i
	"$GOMAKE" clean
	"$GOMAKE"
	) || exit $?
done

$BROKEN ||
[ "$GOARCH" == arm ] ||
(xcd ../test/bench/shootout
./timing.sh -test
) || exit $?

$BROKEN ||
(xcd ../test/bench/go1
"$GOMAKE" test
) || exit $?

(xcd ../test
./run
) || exit $?

echo
echo ALL TESTS PASSED
