#!/bin/bash
set -eo pipefail

pachctl create repo tables || true
pachctl start commit tables@master

schemas=("tpch_sfmicro" "tpch_sf1" "tpch_sf10")
tables=("customer" "lineitem" "nation" "orders" "part" "partsupp" "region" "supplier")
for schema in ${schemas[@]}; do
    for table in ${tables[@]}; do
        echo "" | pachctl put file "tables@master:/${schema}.${table}"
    done
done

pachctl finish commit tables@master
