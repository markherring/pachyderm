#!/bin/bash
set -eo pipefail

pachctl create repo tables

schemas=("tpch_sf1")
tables=("customer" "lineitem" "nation" "orders" "part" "partsupp" "region" "supplier")
for schema in ${schemas[@]}; do
    for table in ${tables[@]}; do
        echo "" | pachctl put file "tables@master:/${schema}.${table}"
    done
done