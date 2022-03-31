#!/bin/bash
set -xeuo pipefail

# warmup...
BENCH_NUMJOBS=1 BENCH_FILESIZE=1M BENCH_NRFILES=10 ./run.sh
BENCH_NUMJOBS=1 BENCH_FILESIZE=1M BENCH_NRFILES=100 ./run.sh

# large number of small files
BENCH_NUMJOBS=1 BENCH_FILESIZE=1M BENCH_NRFILES=1000 ./run.sh

# smaller number of large files
BENCH_NUMJOBS=1 BENCH_FILESIZE=100M BENCH_NRFILES=100 ./run.sh

# parallelism... disabled because these give 'error=Input/output error'
#BENCH_NUMJOBS=10 BENCH_FILESIZE=1M BENCH_NRFILES=1000 ./run.sh
#BENCH_NUMJOBS=10 BENCH_FILESIZE=1M BENCH_NRFILES=100 ./run.sh

# pump it...
BENCH_NUMJOBS=1 BENCH_FILESIZE=10K BENCH_NRFILES=10000 ./run.sh
BENCH_NUMJOBS=1 BENCH_FILESIZE=100K BENCH_NRFILES=1000 ./run.sh

# huge number of files...
BENCH_NUMJOBS=1 BENCH_FILESIZE=4K BENCH_NRFILES=100000 ./run.sh

# probably overly ambitious...
#BENCH_NUMJOBS=10 BENCH_FILESIZE=4K BENCH_NRFILES=100000 ./run.sh